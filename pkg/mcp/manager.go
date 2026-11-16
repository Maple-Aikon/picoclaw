package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// headerTransport is an http.RoundTripper that adds custom headers to requests
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func expandHomeCommandPath(command string) string {
	if command == "" || command[0] != '~' {
		return command
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return command
	}
	if command == "~" {
		return home
	}
	if strings.HasPrefix(command, "~/") || strings.HasPrefix(command, "~\\") {
		return filepath.Join(home, command[2:])
	}
	return command
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid modifying the original
	req = req.Clone(req.Context())

	// Add custom headers
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}

	// Use the base transport
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// loadEnvFile loads environment variables from a file in .env format
// Each line should be in the format: KEY=value
// Lines starting with # are comments
// Empty lines are ignored
func loadEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open env file: %w", err)
	}
	defer file.Close()

	envVars := make(map[string]string)
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse KEY=value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid format at line %d: %s", lineNum, line)
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if key == "" {
			return nil, fmt.Errorf("invalid format at line %d: empty key", lineNum)
		}

		// Remove surrounding quotes if present
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		envVars[key] = value
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading env file: %w", err)
	}

	return envVars, nil
}

// ServerConnection represents a connection to an MCP server
type ServerConnection struct {
	Name          string
	Config        config.MCPServerConfig
	Client        *mcp.Client
	Session       *mcp.ClientSession
	Tools         []*mcp.Tool
	SubprocessPID int // PID of underlying stdio subprocess (0 = not stdio / not tracked)
	reconnectMu   sync.Mutex

	// P1 (2026-11-16): lifecycle bookkeeping owned by the Manager.
	// startedAt is when the current process was spawned; lastUsed is the
	// last time a tool call actually ran against it. Both drive the reaper.
	//
	// R9 (2026-11-16): these are stored as atomic int64 (UnixNano), NOT as
	// time.Time. A time.Time is 24 bytes (wall + ext + loc) and is NOT
	// written atomically, so a reader could observe a half-written value.
	// touch() writes lastUsed from the tool-call goroutine without holding
	// m.mu, while reapExpired() reads it under m.mu.RLock() - the lock did
	// not protect anything, because the writer never took it. A torn read
	// produces a garbage instant, which either kills a server that was just
	// used or keeps an expired server alive.
	startedAtNano atomic.Int64
	lastUsedNano  atomic.Int64

	// intentionalKill is set when the Manager itself killed the process
	// (idle timeout, lifetime timeout, or Close). It distinguishes "we
	// killed it" from "it died on its own", which decides whether the next
	// tool call reconnects silently or surfaces an error.
	intentionalKill atomic.Bool
	// procExited is closed once the process is confirmed dead.
	procExited chan struct{}
	// transport is the stdio transport that owns the subprocess, kept so
	// the process exit can be observed from more than one place (R13).
	transport *isolatedCommandTransport
}

// Manager manages multiple MCP server connections
type Manager struct {
	servers            map[string]*ServerConnection
	runtimeEvents      runtimeevents.Bus
	mu                 sync.RWMutex
	closed             atomic.Bool    // changed from bool to atomic.Bool to avoid TOCTOU race
	wg                 sync.WaitGroup // tracks in-flight CallTool calls
	defaultCallTimeout time.Duration  // default timeout for tool calls (0 = use fallback)

	// R8 (2026-11-16): the configuration of every known server, kept even
	// while the server is not connected.
	//
	// The reaper removes a server from m.servers when it hits the idle or
	// lifetime timeout. Without this map the Manager could not bring the
	// server back, and every tool of that server would fail forever with
	// "server X not found" until the gateway restarted. That is what the
	// live run on 2026-11-16 at 15:59 actually showed.
	serverConfigs map[string]config.MCPServerConfig

	// P0b (2026-11-16): explicit timeout policy. These bound how long a
	// server process may live and how long a reconnect handshake may take.
	// They are what makes "MCP never runs forever" true, independently of
	// whatever context the caller happens to hold.
	idleTimeout      time.Duration
	maxLifetime      time.Duration
	reconnectTimeout time.Duration

	// reaperStop ends the reaper goroutine when the Manager closes.
	reaperStop chan struct{}
	reaperDone chan struct{}
}

// Default timeout policy for MCP servers. Chosen by Maple on 2026-11-16.
const (
	defaultMCPIdleTimeout      = 10 * time.Minute
	defaultMCPMaxLifetime      = 2 * time.Hour
	defaultMCPReconnectTimeout = 30 * time.Second
	// mcpReaperInterval is how often the reaper scans for expired servers.
	// The effective idle kill window is therefore idle_timeout..idle_timeout+interval.
	mcpReaperInterval = 30 * time.Second
	// mcpKillGrace is how long a killed process group gets to exit politely.
	mcpKillGrace = 5 * time.Second
)

var connectServerFunc = connectServer

// ManagerOption configures an MCP manager.
type ManagerOption func(*Manager)

// WithRuntimeEvents injects the runtime event bus used for MCP observations.
func WithRuntimeEvents(eventBus runtimeevents.Bus) ManagerOption {
	return func(m *Manager) {
		m.runtimeEvents = eventBus
	}
}

// ServerEventPayload describes MCP server connection events.
type ServerEventPayload struct {
	Server    string `json:"server"`
	Type      string `json:"type,omitempty"`
	URL       string `json:"url,omitempty"`
	Command   string `json:"command,omitempty"`
	Tool      string `json:"tool,omitempty"`
	ToolCount int    `json:"tool_count,omitempty"`
	Error     string `json:"error,omitempty"`
}

// NewManager creates a new MCP manager
func NewManager(opts ...ManagerOption) *Manager {
	m := &Manager{
		servers:          make(map[string]*ServerConnection),
		serverConfigs:    make(map[string]config.MCPServerConfig),
		idleTimeout:      defaultMCPIdleTimeout,
		maxLifetime:      defaultMCPMaxLifetime,
		reconnectTimeout: defaultMCPReconnectTimeout,
		reaperStop:       make(chan struct{}),
		reaperDone:       make(chan struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	go m.reaper()
	return m
}

// LoadFromConfig loads MCP servers from configuration
func (m *Manager) LoadFromConfig(ctx context.Context, cfg *config.Config) error {
	return m.LoadFromMCPConfig(ctx, cfg.Tools.MCP, cfg.WorkspacePath())
}

// LoadFromMCPConfig loads MCP servers from MCP configuration and workspace path.
// This is the minimal dependency version that doesn't require the full Config object.
func (m *Manager) LoadFromMCPConfig(
	ctx context.Context,
	mcpCfg config.MCPConfig,
	workspacePath string,
) error {
	if !mcpCfg.Enabled {
		logger.InfoCF("mcp", "MCP integration is disabled", nil)
		return nil
	}

	// Parse default call timeout
	m.defaultCallTimeout = parseTimeout(mcpCfg.DefaultCallTimeout)

	// P0b (2026-11-16): parse the explicit timeout policy. An empty string
	// means "use the built-in default"; an explicit "0" means "disabled".
	m.idleTimeout = resolvePolicyTimeout(mcpCfg.IdleTimeout, defaultMCPIdleTimeout)
	m.maxLifetime = resolvePolicyTimeout(mcpCfg.MaxLifetime, defaultMCPMaxLifetime)
	m.reconnectTimeout = resolvePolicyTimeout(mcpCfg.ReconnectTimeout, defaultMCPReconnectTimeout)

	logger.InfoCF("mcp", "MCP timeout policy resolved",
		map[string]any{
			"idle_timeout":      m.idleTimeout.String(),
			"max_lifetime":      m.maxLifetime.String(),
			"reconnect_timeout": m.reconnectTimeout.String(),
		})

	if len(mcpCfg.Servers) == 0 {
		logger.InfoCF("mcp", "No MCP servers configured", nil)
		return nil
	}

	logger.InfoCF("mcp", "Initializing MCP servers",
		map[string]any{
			"count": len(mcpCfg.Servers),
		})

	var wg sync.WaitGroup
	errs := make(chan error, len(mcpCfg.Servers))
	enabledCount := 0

	for name, serverCfg := range mcpCfg.Servers {
		if !serverCfg.Enabled {
			logger.DebugCF("mcp", "Skipping disabled server",
				map[string]any{
					"server": name,
				})
			continue
		}

		enabledCount++
		wg.Add(1)
		go func(name string, serverCfg config.MCPServerConfig, workspace string) {
			defer wg.Done()

			// Resolve relative envFile paths relative to workspace
			if serverCfg.EnvFile != "" && !filepath.IsAbs(serverCfg.EnvFile) {
				if workspace == "" {
					err := fmt.Errorf(
						"workspace path is empty while resolving relative envFile %q for server %s",
						serverCfg.EnvFile,
						name,
					)
					logger.ErrorCF("mcp", "Invalid MCP server configuration",
						map[string]any{
							"server":   name,
							"env_file": serverCfg.EnvFile,
							"error":    err.Error(),
						})
					errs <- err
					return
				}
				serverCfg.EnvFile = filepath.Join(workspace, serverCfg.EnvFile)
			}

			if err := m.ConnectServer(ctx, name, serverCfg); err != nil {
				logger.ErrorCF("mcp", "Failed to connect to MCP server",
					map[string]any{
						"server": name,
						"error":  err.Error(),
					})
				errs <- fmt.Errorf("failed to connect to server %s: %w", name, err)
			}
		}(name, serverCfg, workspacePath)
	}

	wg.Wait()
	close(errs)

	// Collect errors
	var allErrors []error
	for err := range errs {
		allErrors = append(allErrors, err)
	}

	connectedCount := len(m.GetServers())

	// If all enabled servers failed to connect, return aggregated error
	if enabledCount > 0 && connectedCount == 0 {
		logger.ErrorCF("mcp", "All MCP servers failed to connect",
			map[string]any{
				"failed": len(allErrors),
				"total":  enabledCount,
			})
		return errors.Join(allErrors...)
	}

	if len(allErrors) > 0 {
		logger.WarnCF("mcp", "Some MCP servers failed to connect",
			map[string]any{
				"failed":    len(allErrors),
				"connected": connectedCount,
				"total":     enabledCount,
			})
		// Don't fail completely if some servers successfully connected
	}

	logger.InfoCF("mcp", "MCP server initialization complete",
		map[string]any{
			"connected": connectedCount,
			"total":     enabledCount,
		})

	return nil
}

// ConnectServer connects to a single MCP server
func (m *Manager) ConnectServer(
	ctx context.Context,
	name string,
	cfg config.MCPServerConfig,
) error {
	m.publishServerEvent(runtimeevents.KindMCPServerConnecting, name, cfg, 0, nil)
	conn, err := connectServerFunc(ctx, name, cfg)
	if err != nil {
		m.publishServerEvent(runtimeevents.KindMCPServerFailed, name, cfg, 0, err)
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed.Load() {
		_ = conn.Session.Close()
		m.publishServerEvent(runtimeevents.KindMCPServerFailed, name, cfg, 0, fmt.Errorf("manager is closed"))
		return fmt.Errorf("manager is closed")
	}

	m.servers[name] = conn
	// R8 (2026-11-16): remember the configuration so the server can be
	// re-spawned later, even after the reaper removed it from m.servers.
	m.serverConfigs[name] = cfg
	for _, tool := range conn.Tools {
		toolName := ""
		if tool != nil {
			toolName = tool.Name
		}
		m.publishToolDiscovered(name, cfg, toolName)
	}
	m.publishServerEvent(runtimeevents.KindMCPServerConnected, name, cfg, len(conn.Tools), nil)
	return nil
}

func connectServer(
	ctx context.Context,
	name string,
	cfg config.MCPServerConfig,
) (*ServerConnection, error) {
	logger.InfoCF("mcp", "Connecting to MCP server",
		map[string]any{
			"server":     name,
			"command":    cfg.Command,
			"args_count": len(cfg.Args),
		})

	// Create client
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "picoclaw",
		Version: "1.0.0",
	}, nil)

	// Create transport based on configuration
	// Auto-detect transport type if not explicitly specified
	var transport mcp.Transport
	transportType := config.EffectiveMCPTransportType(cfg)
	if transportType == "" {
		return nil, fmt.Errorf("either URL or command must be provided")
	}

	switch transportType {
	case "sse", "http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("URL is required for SSE/HTTP transport")
		}

		// Configure DisableStandaloneSSE based on transport type.
		// - "http": Streamable HTTP request-response mode. Disable the standalone
		//   SSE stream to avoid compatibility issues with servers that don't
		//   support the optional GET listener.
		// - "sse": Bidirectional mode. Enable the standalone SSE stream to receive
		//   server-initiated notifications (e.g., ToolListChangedNotification).
		// - Empty or auto-detected: Defaults to "sse" behavior (standalone SSE enabled).
		disableStandaloneSSE := transportType == "http"

		logger.DebugCF("mcp", "Using SSE/HTTP transport",
			map[string]any{
				"server":               name,
				"url":                  cfg.URL,
				"disableStandaloneSSE": disableStandaloneSSE,
			})

		sseTransport := &mcp.StreamableClientTransport{
			Endpoint:             cfg.URL,
			DisableStandaloneSSE: disableStandaloneSSE,
		}

		// Add custom headers if provided
		if len(cfg.Headers) > 0 {
			// Create a custom HTTP client with header-injecting transport
			sseTransport.HTTPClient = &http.Client{
				Transport: &headerTransport{
					base:    http.DefaultTransport,
					headers: cfg.Headers,
				},
			}
			logger.DebugCF("mcp", "Added custom HTTP headers",
				map[string]any{
					"server":       name,
					"header_count": len(cfg.Headers),
				})
		}

		transport = sseTransport
	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("command is required for stdio transport")
		}
		logger.DebugCF("mcp", "Using stdio transport",
			map[string]any{
				"server":  name,
				"command": cfg.Command,
			})
		// P0a (2026-11-16): the subprocess lifetime is owned by the Manager,
		// NOT by the caller's context.
		//
		// Before this fix the command was built with exec.CommandContext(ctx, …),
		// which makes os/exec install a watchCtx goroutine that SIGKILLs the
		// process the moment ctx is canceled (Go os/exec exec.go:484/780).
		// The ctx that reaches here is the caller's ctx: a tool-call ctx on
		// reconnect (manager.go:656) and a 30s reloadCtx on config reload
		// (gateway.go:654). Either way the MCP process died seconds after it
		// was spawned, the stdout pipe EOFed, and the SDK reported
		// "client is closing: EOF".
		//
		// Measured live before the fix: 105 "session was lost" events, 104 of
		// them on context-mode (DIRECT node) and 1 on codebase-memory
		// (DIRECT C binary) - every WRAPPER server (uv/uvx/npx) was at 0,
		// because killing a wrapper leaves its child holding the pipe open.
		//
		// ctx is still honored for the handshake below (client.Connect +
		// listServerTools): if the handshake hangs, it aborts. Only the
		// process lifetime is decoupled.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("context already done before spawning %s: %w", name, err)
		}
		cmd := exec.Command(expandHomeCommandPath(cfg.Command), cfg.Args...)
		// P1 (2026-11-16): own process group so killing the group also kills
		// wrapper children (uv/npx) that would otherwise be orphaned.
		configureProcessGroup(cmd)

		// Build environment variables with proper override semantics
		// Use a map to ensure config variables override file variables
		envMap := make(map[string]string)

		// Start with parent process environment
		for _, e := range cmd.Environ() {
			if idx := strings.Index(e, "="); idx > 0 {
				envMap[e[:idx]] = e[idx+1:]
			}
		}

		// Load environment variables from file if specified
		if cfg.EnvFile != "" {
			envVars, err := loadEnvFile(cfg.EnvFile)
			if err != nil {
				return nil, fmt.Errorf("failed to load env file %s: %w", cfg.EnvFile, err)
			}
			for k, v := range envVars {
				envMap[k] = v
			}
			logger.DebugCF("mcp", "Loaded environment variables from file",
				map[string]any{
					"server":    name,
					"envFile":   cfg.EnvFile,
					"var_count": len(envVars),
				})
		}

		// Environment variables from config override those from file
		for k, v := range cfg.Env {
			envMap[k] = v
		}

		// Convert map to slice
		env := make([]string, 0, len(envMap))
		for k, v := range envMap {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
		transport = &isolatedCommandTransport{Command: cmd}
	default:
		return nil, fmt.Errorf(
			"unsupported transport type: %s (supported: stdio, sse, http, streamable-http)",
			transportType,
		)
	}

	// Connect to server
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	// Get server info
	initResult := session.InitializeResult()
	logger.InfoCF("mcp", "Connected to MCP server",
		map[string]any{
			"server":        name,
			"serverName":    initResult.ServerInfo.Name,
			"serverVersion": initResult.ServerInfo.Version,
			"protocol":      initResult.ProtocolVersion,
		})

	// List available tools if supported
	tools, err := listServerTools(ctx, name, session, initResult)
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	var subprocessPID int
	if ict, ok := transport.(*isolatedCommandTransport); ok && ict.Command != nil && ict.Command.Process != nil {
		subprocessPID = ict.Command.Process.Pid
	}
	now := time.Now()
	conn := &ServerConnection{
		Name:          name,
		Config:        cfg,
		Client:        client,
		Session:       session,
		Tools:         tools,
		SubprocessPID: subprocessPID,
		procExited:    make(chan struct{}),
	}
	// R13 (2026-11-16): keep the transport so the exit result can be
	// observed from more than one place (the Manager's waiter and any
	// test that wants to watch the process exit).
	if ict, ok := transport.(*isolatedCommandTransport); ok {
		conn.transport = ict
	}
	conn.startedAtNano.Store(now.UnixNano())
	conn.lastUsedNano.Store(now.UnixNano())

	// P1 (2026-11-16): observe the process exit so the connection knows
	// whether the subprocess is still alive.
	//
	// cmd.Wait() is owned by the transport (see
	// isolatedCommandTransport.startWaiter) - Go's os/exec allows exactly one
	// Wait per Cmd, and a second call fails with "waitid: no child
	// processes". We observe the transport's wait channel instead of
	// calling Wait() again.
	//
	// Before the fix cmd.Wait() ran only from isolatedPipeRWC.Close(), so a
	// process killed by any other path stayed a zombie forever. Measured
	// live: 6 "[uv] <defunct>" zombies accumulated under the gateway.
	if ict, ok := transport.(*isolatedCommandTransport); ok && ict.Command != nil {
		waitForProcessExit(ict, conn)
	}
	return conn, nil
}

// waitForProcessExit closes conn.procExited once the subprocess is gone.
//
// Callers use it to distinguish "process still running" from "process
// already dead" without relying on /proc, which reports zombies as present
// and that made an earlier version of this code report dead processes as
// alive (contract C8).
//
// R10 (2026-11-16): the wait must NOT be bounded by a short timeout. The
// previous version gave up after 60 seconds, which is shorter than the
// idle timeout (10m) and the max lifetime (2h) - so for every server that
// was reaped by the policy, the waiter had ALREADY timed out and gave up
// long before the kill happened. The consequences were measured live on
// 2026-11-16:
//
//	15:45:31  server spawned
//	15:46:31  waiter gave up (60s timeout) - the process is still alive
//	15:56:01  idle_timeout kill -> NO "MCP subprocess exited" row,
//	          procExited never closed, isProcessAlive() lied.
func waitForProcessExit(ict *isolatedCommandTransport, conn *ServerConnection) {
	if ict == nil || conn == nil {
		return
	}
	go func() {
		// No timeout: the waiter lives as long as the subprocess does.
		// The transport's waitCh holds the result, and waitResult is
		// tolerant of repeated calls, so this goroutine parks here until
		// the process really exits.
		err, ok := ict.waitResult(0)
		if !ok {
			return
		}
		if conn.procExited != nil {
			close(conn.procExited)
		}
		logger.InfoCF("mcp", "MCP subprocess exited",
			map[string]any{
				"server":           conn.Name,
				"pid":              conn.SubprocessPID,
				"err":              errString(err),
				"intentional_kill": conn.intentionalKill.Load(),
				"lifetime_ms":      time.Since(conn.startedAt()).Milliseconds(),
			})
	}()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// GetServers returns all connected servers
func (m *Manager) GetServers() map[string]*ServerConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]*ServerConnection, len(m.servers))
	for k, v := range m.servers {
		result[k] = v
	}
	return result
}

// GetServer returns a specific server connection
func (m *Manager) GetServer(name string) (*ServerConnection, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	conn, ok := m.servers[name]
	return conn, ok
}

// CallTool calls a tool on a specific server
func (m *Manager) CallTool(
	ctx context.Context,
	serverName, toolName string,
	arguments map[string]any,
) (*mcp.CallToolResult, error) {
	// Check if closed before acquiring lock (fast path)
	if m.closed.Load() {
		return nil, fmt.Errorf("manager is closed")
	}

	m.mu.RLock()
	// Double-check after acquiring lock to prevent TOCTOU race
	if m.closed.Load() {
		m.mu.RUnlock()
		return nil, fmt.Errorf("manager is closed")
	}
	conn, ok := m.servers[serverName]
	if ok {
		m.wg.Add(1) // Add to WaitGroup while holding the lock
	}
	m.mu.RUnlock()

	if !ok {
		// R8 (2026-11-16): the server is not connected. That is NOT an
		// error the caller should see: the reaper removes idle servers on
		// purpose, and the next tool call has to bring them back.
		//
		// Measured live on 2026-11-16: after the idle-timeout kill, a tool
		// call failed with "MCP tool execution failed: server planning-mcp
		// not found" - and every later call would fail the same way, because
		// nothing else ever re-created the server.
		//
		// The re-spawn must NOT inherit the caller's context: that was the
		// original bug (P0a). The handshake gets its own bounded context.
		reconnected, err := m.lazyReconnect(ctx, serverName)
		if err != nil {
			return nil, fmt.Errorf("failed to reconnect MCP server %s: %w", serverName, err)
		}
		conn = reconnected
		m.wg.Add(1)
	}
	defer m.wg.Done()

	// Resolve timeout for this tool call
	timeout := resolveTimeout(conn.Config, m.defaultCallTimeout, toolName)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// P0b (2026-11-16): record activity so the reaper's idle timer starts
	// from the last real use, not from the moment the server was spawned.
	conn.touch()

	params := &mcp.CallToolParams{
		Name:      toolName,
		Arguments: arguments,
	}

	// Preemptive watchdog: force-kill the subprocess if CallTool doesn't return
	// within timeout + 5s grace period. The 5s grace is intentional — it gives
	// the MCP subprocess time to flush + self-exit on the SDK's notifications/canceled
	// path (which fires at `timeout` via callCtx cancellation) before we force-close.
	// If the subprocess ignores cancellation and hangs, the force-close at +5s
	// guarantees we always recover within bounded time.
	watchConn := conn
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-time.After(timeout + 5*time.Second):
			pid := watchConn.SubprocessPID
			logger.WarnCF("mcp", "preemptive watchdog fired, force-killing MCP subprocess",
				map[string]any{
					"server":  serverName,
					"tool":    toolName,
					"timeout": timeout.String(),
					"pid":     pid,
				})
			// Direct force-kill bypasses any stuck SDK state. Safe — even if it
			// didn't hang on I/O, killing the subprocess causes pipe EOF, which
			// unblocks the SDK's read loop. On Unix os.Process.Kill() signals
			// SIGKILL; on Windows it calls TerminateProcess.
			if pid > 0 {
				proc, err := os.FindProcess(pid)
				if err != nil {
					logger.ErrorCF("mcp", "find process failed, cannot kill",
						map[string]any{"pid": pid, "err": err.Error()})
				} else if err := proc.Kill(); err != nil {
					logger.ErrorCF("mcp", "direct process kill failed, falling back",
						map[string]any{"pid": pid, "err": err.Error()})
				} else {
					logger.InfoCF("mcp", "direct process kill succeeded",
						map[string]any{"pid": pid, "server": serverName})
				}
			}
			// Always call Close to release SDK resources. Safe to call
			// multiple times; Close is idempotent (uses closeOnce).
			_ = watchConn.Session.Close()
		case <-done:
		}
	}()

	result, err := conn.Session.CallTool(callCtx, params)
	if err != nil {
		// Detect timeout and treat as a connection-health issue
		if errors.Is(err, context.DeadlineExceeded) {
			logger.WarnCF("mcp", "MCP tool call timed out, triggering reconnect",
				map[string]any{
					"server":  serverName,
					"tool":    toolName,
					"timeout": timeout.String(),
				})

			// Early kill: close the stale session immediately to kill the hanging
			// subprocess, preventing process leaks during the grace period.
			_ = conn.Session.Close()

			// Give the SDK's notifications/canceled time to reach the server
			// before closing the connection via reconnectServer.
			// If the parent context is already done, abort immediately.
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("tool %q call failed: parent context done: %w", toolName, ctx.Err())
			case <-time.After(3 * time.Second):
			}

			// Reconnect with its own bounded timeout - never use parent ctx (may have
			// no deadline, or be canceled already). Cap at the configured
			// reconnect timeout to avoid hanging on a stuck subprocess during
			// reconnect (covers cold start, FS lock, etc.).
			//
			// P0a (2026-11-16): the cancel is deferred, not called immediately.
			// Cancelling the handshake ctx right after reconnectServer returns
			// would kill the process we just spawned.
			reconnectCtx, reconnectCancel := context.WithTimeout(context.Background(), m.reconnectTimeout)
			defer reconnectCancel()
			reconnectedConn, reconnectErr := m.reconnectServer(reconnectCtx, serverName, conn)
			if reconnectErr != nil {
				return nil, fmt.Errorf("tool %q timed out after %v and reconnect failed: %w",
					toolName, timeout, reconnectErr)
			}
			// Update conn reference for the retry
			conn = reconnectedConn
			// Retry once with a fresh timeout context tied to Background, not parent
			// ctx — parent ctx may already be canceled, but the retry should still
			// have its full timeout window since the user paid for it.
			retryCtx, retryCancel := context.WithTimeout(context.Background(), timeout)
			defer retryCancel()
			result, err = conn.Session.CallTool(retryCtx, params)
			if err != nil {
				return nil, fmt.Errorf("tool %q timed out after %v (retry also failed: %w)",
					toolName, timeout, err)
			}
			return result, nil
		}

		if shouldReconnectCallError(err) {
			// P0c (2026-11-16): instrument the cause BEFORE reconnecting.
			// Without this, "EOF" has no attribution and every fix is a guess
			// (contract C6). We report whether the subprocess was still
			// alive, whether we killed it ourselves, and how old it was.
			m.logSessionLoss(serverName, toolName, conn, err)

			logger.WarnCF("mcp", "MCP server session was lost during tool call, reconnecting",
				map[string]any{
					"server": serverName,
					"tool":   toolName,
					"error":  err.Error(),
				})

			// P0a (2026-11-16): the reconnect must NOT inherit the tool-call
			// context. Before the fix this passed the caller's ctx straight
			// into connectServer, so the freshly spawned process was bound
			// to a context that was canceled the moment the tool call
			// returned - the process was SIGKILLed seconds after it started.
			//
			// The reconnect gets its own bounded context instead: it may
			// outlive the tool call (that is the point) but it is still
			// bounded by the configured reconnect timeout.
			//
			// IMPORTANT: since P0a the subprocess is NOT bound to this ctx
			// any more (connectServer uses exec.Command, not
			// exec.CommandContext), so cancelling the handshake ctx can no
			// longer kill the process. Deferring the cancel is therefore
			// safe AND it releases the timer promptly.
			reconnectCtx, reconnectCancel := context.WithTimeout(
				context.Background(), m.reconnectTimeout)
			defer reconnectCancel()
			reconnectedConn, reconnectErr := m.reconnectServer(reconnectCtx, serverName, conn)
			if reconnectErr != nil {
				return nil, fmt.Errorf("failed to recover lost MCP session: %w", reconnectErr)
			}

			result, err = reconnectedConn.Session.CallTool(ctx, params)
			if err == nil {
				return result, nil
			}
		}

		return nil, fmt.Errorf("failed to call tool: %w", err)
	}

	return result, nil
}

func listServerTools(
	ctx context.Context,
	name string,
	session *mcp.ClientSession,
	initResult *mcp.InitializeResult,
) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	if initResult.Capabilities.Tools == nil {
		return tools, nil
	}

	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			logger.WarnCF("mcp", "Error listing tool",
				map[string]any{
					"server": name,
					"error":  err.Error(),
				})
			continue
		}
		tools = append(tools, tool)
	}

	logger.InfoCF("mcp", "Listed tools from MCP server",
		map[string]any{
			"server":    name,
			"toolCount": len(tools),
		})

	return tools, nil
}

func shouldReconnectCallError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mcp.ErrSessionMissing) {
		return true
	}

	errStr := strings.ToLower(err.Error())
	patterns := []string{
		mcp.ErrSessionMissing.Error(),
		"connection closed",
		"eof",
		"broken pipe",
		"client is closing",
	}

	for _, p := range patterns {
		if strings.Contains(errStr, p) {
			return true
		}
	}

	return false
}

// defaultToolCallTimeout is the fallback when no config is set.
const defaultToolCallTimeout = 60 * time.Second

// parseTimeout parses a duration string, returning 0 on failure.
func parseTimeout(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// resolveTimeout picks the effective timeout for a tool call.
// Priority: per-tool > per-server > global default > hardcoded fallback (60s).
func resolveTimeout(
	serverConfig config.MCPServerConfig,
	globalDefault time.Duration,
	toolName string,
) time.Duration {
	// Layer 1: Per-tool timeout
	if serverConfig.ToolTimeouts != nil {
		if t, ok := serverConfig.ToolTimeouts[toolName]; ok && t != "" {
			if d := parseTimeout(t); d > 0 {
				return d
			}
		}
	}

	// Layer 2: Per-server timeout
	if serverConfig.CallTimeout != "" {
		if d := parseTimeout(serverConfig.CallTimeout); d > 0 {
			return d
		}
	}

	// Layer 3: Global default timeout
	if globalDefault > 0 {
		return globalDefault
	}

	// Layer 4: Hardcoded fallback
	return defaultToolCallTimeout
}

// resolvePolicyTimeout parses a policy duration.
//
// Semantics:
//   - ""  -> use the built-in default (the policy is enabled)
//   - "0" -> the policy is explicitly DISABLED for this scope
//   - anything else that parses -> that duration
//   - unparseable -> the built-in default (fail safe: keep the guard on)
func resolvePolicyTimeout(raw string, def time.Duration) time.Duration {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return def
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		logger.WarnCF("mcp", "Invalid timeout policy value, using default",
			map[string]any{"value": raw, "default": def.String()})
		return def
	}
	if d < 0 {
		return def
	}
	return d
}

// serverIdleTimeout resolves the idle timeout for one server, letting the
// per-server setting override the global one.
func (m *Manager) serverIdleTimeout(conn *ServerConnection) time.Duration {
	if conn == nil {
		return m.idleTimeout
	}
	if v := strings.TrimSpace(conn.Config.IdleTimeout); v != "" {
		return resolvePolicyTimeout(v, m.idleTimeout)
	}
	return m.idleTimeout
}

// serverMaxLifetime resolves the max lifetime for one server, letting the
// per-server setting override the global one.
func (m *Manager) serverMaxLifetime(conn *ServerConnection) time.Duration {
	if conn == nil {
		return m.maxLifetime
	}
	if v := strings.TrimSpace(conn.Config.MaxLifetime); v != "" {
		return resolvePolicyTimeout(v, m.maxLifetime)
	}
	return m.maxLifetime
}

// reaper enforces the timeout policy: it kills servers that sat idle for
// too long, and servers that have been alive for longer than their maximum
// lifetime. Without this, "MCP never runs forever" was only true because
// something else happened to kill the process - never by design.
//
// The kill removes the server from the map, so the next tool call lazily
// reconnects. That reconnect is invisible to the user apart from a short
// cold-start delay.
func (m *Manager) reaper() {
	defer close(m.reaperDone)
	ticker := time.NewTicker(mcpReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.reaperStop:
			return
		case <-ticker.C:
			m.reapExpired()
		}
	}
}

// reapExpired scans every tracked server and kills the expired ones.
func (m *Manager) reapExpired() {
	if m.closed.Load() {
		return
	}
	now := time.Now()

	type victim struct {
		name string
		conn *ServerConnection
		why  string
	}
	var victims []victim

	m.mu.RLock()
	for name, conn := range m.servers {
		if conn == nil {
			continue
		}
		if idle := m.serverIdleTimeout(conn); idle > 0 && !conn.startedAt().IsZero() {
			// lastUsed is bumped on every tool call; fall back to startedAt
			// so a server that was never called is still measured.
			if conn.idleSince() > idle {
				victims = append(victims, victim{name, conn, "idle_timeout"})
				continue
			}
		}
		if maxLife := m.serverMaxLifetime(conn); maxLife > 0 && !conn.startedAt().IsZero() {
			if now.Sub(conn.startedAt()) > maxLife {
				victims = append(victims, victim{name, conn, "max_lifetime"})
			}
		}
	}
	m.mu.RUnlock()

	for _, v := range victims {
		m.killServer(v.name, v.conn, v.why)
	}
}

// killServer terminates a server process (and its process group) and drops it
// from the map so the next tool call reconnects cleanly.
func (m *Manager) killServer(name string, conn *ServerConnection, reason string) {
	if conn == nil {
		return
	}
	conn.intentionalKill.Store(true)

	// Remove it from the map first so no new call can grab it.
	m.mu.Lock()
	if current, ok := m.servers[name]; ok && current == conn {
		delete(m.servers, name)
	}
	m.mu.Unlock()

	pid := conn.SubprocessPID
	logger.InfoCF("mcp", "Killing MCP server by timeout policy",
		map[string]any{
			"server":      name,
			"reason":      reason,
			"pid":         pid,
			"idle_for_ms": conn.idleSince().Milliseconds(),
			"lifetime_ms": time.Since(conn.startedAt()).Milliseconds(),
		})

	// Kill the whole process group so wrapper children die too.
	killProcessGroup(pid, mcpKillGrace)

	// Close the session to release SDK resources and close the pipes.
	_ = conn.Session.Close()
}

// lazyReconnect re-spawns a server that is not currently connected.
//
// R8 (2026-11-16). The reaper removes idle servers from m.servers on
// purpose. Without this path every tool of a reaped server would fail
// forever with "server X not found" - measured live on 2026-11-16.
//
// The handshake context is derived from Background with the configured
// reconnect timeout, NOT from the caller's context. Binding the new
// process to the caller's context is exactly the bug P0a fixed: the
// process was SIGKILLed the moment the tool call returned.
func (m *Manager) lazyReconnect(ctx context.Context, serverName string) (*ServerConnection, error) {
	if m.closed.Load() {
		return nil, fmt.Errorf("manager is closed")
	}

	m.mu.RLock()
	cfg, known := m.serverConfigs[serverName]
	// A concurrent call may have re-connected the server already.
	if conn, ok := m.servers[serverName]; ok {
		m.mu.RUnlock()
		return conn, nil
	}
	m.mu.RUnlock()

	if !known {
		return nil, fmt.Errorf("server %s not found", serverName)
	}

	// P0a: the spawn must outlive the caller's context.
	handshakeCtx, cancel := context.WithTimeout(context.Background(), m.reconnectTimeout)
	defer cancel()

	conn, err := connectServerFunc(handshakeCtx, serverName, cfg)
	if err != nil {
		return nil, fmt.Errorf("reconnect handshake failed: %w", err)
	}

	m.mu.Lock()
	if m.closed.Load() {
		m.mu.Unlock()
		_ = conn.Session.Close()
		return nil, fmt.Errorf("manager is closed")
	}
	// Another goroutine may have connected it in the meantime - keep that
	// one and drop ours, so there is never more than one process per server.
	if existing, ok := m.servers[serverName]; ok {
		m.mu.Unlock()
		_ = conn.Session.Close()
		return existing, nil
	}
	m.servers[serverName] = conn
	m.mu.Unlock()

	logger.InfoCF("mcp", "Reconnected MCP server on demand",
		map[string]any{
			"server": serverName,
			"pid":    conn.SubprocessPID,
			"reason": "not_connected",
		})
	for _, tool := range conn.Tools {
		toolName := ""
		if tool != nil {
			toolName = tool.Name
		}
		m.publishToolDiscovered(serverName, cfg, toolName)
	}
	m.publishServerEvent(runtimeevents.KindMCPServerConnected, serverName, cfg, len(conn.Tools), nil)

	return conn, nil
}

// reconnectServer re-creates a connection for a server whose session died.
func (m *Manager) reconnectServer(
	ctx context.Context,
	serverName string,
	staleConn *ServerConnection,
) (*ServerConnection, error) {
	if staleConn == nil {
		return nil, fmt.Errorf("server %s not found", serverName)
	}

	staleConn.reconnectMu.Lock()
	defer staleConn.reconnectMu.Unlock()

	if m.closed.Load() {
		return nil, fmt.Errorf("manager is closed")
	}

	m.mu.RLock()
	currentConn, ok := m.servers[serverName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("server %s not found", serverName)
	}
	if currentConn != staleConn {
		return currentConn, nil
	}

	freshConn, err := connectServerFunc(ctx, serverName, staleConn.Config)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed.Load() {
		m.mu.Unlock()
		_ = freshConn.Session.Close()
		return nil, fmt.Errorf("manager is closed")
	}

	currentConn, ok = m.servers[serverName]
	if !ok {
		m.mu.Unlock()
		_ = freshConn.Session.Close()
		return nil, fmt.Errorf("server %s not found", serverName)
	}

	if currentConn == staleConn {
		m.servers[serverName] = freshConn
		staleToClose := staleConn
		m.mu.Unlock()
		_ = staleToClose.Session.Close()
		return freshConn, nil
	}

	m.mu.Unlock()
	_ = freshConn.Session.Close()
	return currentConn, nil
}

// touch records that the server was just used, resetting the idle timer.
//
// R9 (2026-11-16): the timestamp is stored atomically. A plain
// time.Time field would be written non-atomically while the reaper reads
// it concurrently, which can produce a torn (garbage) instant.
func (c *ServerConnection) touch() {
	if c == nil {
		return
	}
	c.lastUsedNano.Store(time.Now().UnixNano())
}

// startedAt returns the spawn time of the current subprocess.
func (c *ServerConnection) startedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	n := c.startedAtNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// lastUsed returns the time of the last tool call against this server.
func (c *ServerConnection) lastUsed() time.Time {
	if c == nil {
		return time.Time{}
	}
	n := c.lastUsedNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// idleSince returns how long the server has been idle.
//
// It falls back to the spawn time when the server was never called, so a
// freshly spawned server that nobody uses still ages out. The result is
// never negative: a clock adjustment backwards would otherwise make the
// reaper believe a just-used server has been idle for hours.
func (c *ServerConnection) idleSince() time.Duration {
	if c == nil {
		return 0
	}
	last := c.lastUsed()
	if last.IsZero() {
		last = c.startedAt()
	}
	if last.IsZero() {
		return 0
	}
	idle := time.Since(last)
	if idle < 0 {
		return 0
	}
	return idle
}

// isProcessAlive reports whether the subprocess is still running.
//
// It waits on procExited (closed by the waiter goroutine when the process
// dies) instead of checking /proc, because /proc lists zombies as present
// and that made an earlier version of this code report dead processes as
// alive (contract C8).
func (c *ServerConnection) isProcessAlive() bool {
	if c == nil {
		return false
	}
	if c.SubprocessPID <= 0 {
		return false
	}
	if c.procExited == nil {
		return true
	}
	select {
	case <-c.procExited:
		return false
	default:
		return true
	}
}

// logSessionLoss is the P0c instrumentation: it attributes a session loss to
// a cause so that "EOF" stops being an unattributed symptom.
//
// Causes reported:
//   - "process_dead"          the subprocess exited on its own
//   - "process_alive"         the subprocess is still running, so the pipe
//     broke for another reason (server-side close, protocol error)
//   - "killed_by_policy"      the Manager killed it (idle/lifetime)
//   - "not_stdio"             no tracked subprocess (HTTP/SSE transport)
func (m *Manager) logSessionLoss(
	serverName, toolName string,
	conn *ServerConnection,
	cause error,
) {
	fields := map[string]any{
		"server": serverName,
		"tool":   toolName,
		"error":  cause.Error(),
	}
	if conn == nil {
		fields["cause"] = "unknown_no_connection"
		logger.WarnCF("mcp", "MCP session loss cause", fields)
		return
	}
	if conn.SubprocessPID <= 0 {
		fields["cause"] = "not_stdio"
		logger.WarnCF("mcp", "MCP session loss cause", fields)
		return
	}
	fields["pid"] = conn.SubprocessPID
	fields["lifetime_ms"] = time.Since(conn.startedAt()).Milliseconds()
	fields["idle_ms"] = conn.idleSince().Milliseconds()
	fields["intentional_kill"] = conn.intentionalKill.Load()
	if !conn.isProcessAlive() {
		fields["cause"] = "process_dead"
	} else if conn.intentionalKill.Load() {
		fields["cause"] = "killed_by_policy"
	} else {
		fields["cause"] = "process_alive"
	}
	logger.WarnCF("mcp", "MCP session loss cause", fields)
}

// Close closes all server connections
func (m *Manager) Close() error {
	// Use Swap to atomically set closed=true and get the previous value
	// This prevents TOCTOU race with CallTool's closed check
	if m.closed.Swap(true) {
		return nil // already closed
	}

	// Stop the reaper so it does not race with the teardown below.
	if m.reaperStop != nil {
		close(m.reaperStop)
	}
	if m.reaperDone != nil {
		<-m.reaperDone
	}

	// Wait for all in-flight CallTool calls to finish before closing sessions
	// After closed=true is set, no new CallTool can start (they check closed first)
	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	logger.InfoCF("mcp", "Closing all MCP server connections",
		map[string]any{
			"count": len(m.servers),
		})

	var errs []error
	for name, conn := range m.servers {
		// P1 (2026-11-16): kill the whole process group, not just the
		// session. Session.Close() only closes the pipes; wrapper children
		// (uv/npx) would survive as orphans.
		intentional := false
		if conn != nil {
			conn.intentionalKill.Store(true)
			intentional = true
			killProcessGroup(conn.SubprocessPID, mcpKillGrace)
		}
		if err := conn.Session.Close(); err != nil {
			// We killed the process ourselves, so the SDK reports the
			// termination as an error ("signal: terminated"). That is the
			// expected outcome of an intentional shutdown, not a failure.
			if intentional {
				logger.DebugCF("mcp", "MCP session closed after intentional kill",
					map[string]any{
						"server": name,
						"error":  err.Error(),
					})
			} else {
				logger.ErrorCF("mcp", "Failed to close server connection",
					map[string]any{
						"server": name,
						"error":  err.Error(),
					})
				errs = append(errs, fmt.Errorf("server %s: %w", name, err))
			}
		}
	}

	m.servers = make(map[string]*ServerConnection)

	if len(errs) > 0 {
		return fmt.Errorf("failed to close %d server(s): %w", len(errs), errors.Join(errs...))
	}

	return nil
}

// GetAllTools returns all tools from all connected servers
func (m *Manager) GetAllTools() map[string][]*mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string][]*mcp.Tool)
	for name, conn := range m.servers {
		if len(conn.Tools) > 0 {
			result[name] = conn.Tools
		}
	}
	return result
}
