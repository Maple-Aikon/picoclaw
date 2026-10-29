package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/constants"
	"github.com/sipeed/picoclaw/pkg/isolation"
	"github.com/sipeed/picoclaw/pkg/logger"
)

var (
	globalSessionManager = NewSessionManager()
	sessionManagerMu     sync.RWMutex
)

func getSessionManager() *SessionManager {
	sessionManagerMu.RLock()
	defer sessionManagerMu.RUnlock()
	return globalSessionManager
}

type ExecTool struct {
	workingDir          string
	timeout             time.Duration
	denyPatterns        []*regexp.Regexp
	allowPatterns       []*regexp.Regexp
	customAllowPatterns []*regexp.Regexp
	allowedPathPatterns []*regexp.Regexp
	restrictToWorkspace bool
	allowRemote         bool
	sessionManager      *SessionManager
}

// compileCaseInsensitive compiles src as a case-insensitive regular
// expression.
//
// The exec safety guard used to lowercase the COMMAND and match
// case-sensitive patterns against it. Every pattern containing an
// uppercase literal was therefore a DEAD RULE — it could never fire.
// (Measured: `defaultDenyPatterns[16]` = `<<\s*EOF` had been dead since
// commit 01d694b9, 2026-02-18.)
//
// Compiling the PATTERN case-insensitively and matching it against the
// ORIGINAL command fixes the mechanism without mutating the subject:
//
//   - lowercasing the input destroys information the guard needs
//     (`chmod 700` vs `CHMOD 700`);
//   - a lowercased subject makes a case-SENSITIVE pattern unexpressible;
//   - matching the original keeps the other guard checks
//     (absolutePathPattern, path traversal) operating on the true input.
//
// A user can still demand case-sensitivity per pattern with `(?-i)`,
// e.g. `(?i)(?-i)bar` does NOT match "BAR" — verified.
func compileCaseInsensitive(src string) (*regexp.Regexp, error) {
	return regexp.Compile("(?i)" + src)
}

// mustCompileCaseInsensitive is the panic-on-error variant used for the
// built-in deny lists. It mirrors regexp.MustCompile: a built-in pattern
// that does not compile is a programming error in this package, not a
// runtime condition.
//
// Prepending "(?i)" can never turn a valid expression into an invalid one
// (the flag group is valid at any position), so the panic is unreachable
// in practice — but it is kept so that a future edit to a built-in
// pattern fails loudly at start-up instead of silently disabling a rule.
func mustCompileCaseInsensitive(src string) *regexp.Regexp {
	re, err := compileCaseInsensitive(src)
	if err != nil {
		panic(fmt.Sprintf("picoclaw: invalid built-in deny pattern %q: %v", src, err))
	}
	return re
}

// defaultDenyPatterns holds the built-in deny rules.
//
// Every pattern here is compiled case-insensitively (see
// compileCaseInsensitive) so that an uppercase literal such as the
// `EOF` in the heredoc rule can actually fire. The patterns are kept
// in their historical source form; the case-insensitivity is applied
// where the list is assembled, so the literal below stays readable.
var (
	defaultDenyPatterns = []*regexp.Regexp{
		mustCompileCaseInsensitive(`\brm\s+-[rf]{1,2}\b`),
		mustCompileCaseInsensitive(`\bdel\s+/[fq]\b`),
		mustCompileCaseInsensitive(`\brmdir\s+/s\b`),
		// Match disk wiping commands (must be followed by space/args)
		mustCompileCaseInsensitive(
			`(^|[^-\w])\b(format|mkfs|diskpart)\b\s`,
		),
		mustCompileCaseInsensitive(`\bdd\s+if=`),
		// Block writes to block devices (all common naming schemes).
		mustCompileCaseInsensitive(
			`>\s*/dev/(sd[a-z]|hd[a-z]|vd[a-z]|xvd[a-z]|nvme\d|mmcblk\d|loop\d|dm-\d|md\d|sr\d|nbd\d)`,
		),
		mustCompileCaseInsensitive(`\b(shutdown|reboot|poweroff)\b`),
		mustCompileCaseInsensitive(`:\(\)\s*\{.*\};\s*:`),
		mustCompileCaseInsensitive(`\$\([^)]+\)`),
		mustCompileCaseInsensitive(`\$\{[^}]+\}`),
		mustCompileCaseInsensitive("`[^`]+`"),
		mustCompileCaseInsensitive(`\|\s*sh\b`),
		mustCompileCaseInsensitive(`\|\s*bash\b`),
		mustCompileCaseInsensitive(`;\s*rm\s+-[rf]`),
		mustCompileCaseInsensitive(`&&\s*rm\s+-[rf]`),
		mustCompileCaseInsensitive(`\|\|\s*rm\s+-[rf]`),
		// The heredoc deny rule used to live here as the pattern
		// `<<\s*EOF`. It was DEAD since birth (commit 01d694b9,
		// 2026-02-18) because the guard matched patterns against the
		// LOWERCASED command while this pattern requires an UPPERCASE
		// literal.
		//
		// It is now implemented in code instead — see
		// detectDangerousHeredoc. Two reasons it cannot stay a
		// pattern:
		//
		//  1. As a pattern it is bypassable: guardCommand evaluates the
		//     custom allowlist FIRST and only runs the deny loop when the
		//     command was not allow-listed, so `git apply <<EOF` would
		//     pass while `cat <<EOF` is blocked.
		//  2. Making it case-insensitive would make it fire on `<<EOF`
		//     text that sits inside a quoted string. Measured on the
		//     real exec corpus (3098 commands): the pattern matches 3
		//     commands and ALL 3 are inside a double-quoted region
		//     (e.g. an `echo "=== mentioning <<EOF ==="` banner).
		//
		// The code scanner is quote-aware, so it does not fire on those.
		mustCompileCaseInsensitive(`\$\(\s*cat\s+`),
		mustCompileCaseInsensitive(`\$\(\s*curl\s+`),
		mustCompileCaseInsensitive(`\$\(\s*wget\s+`),
		mustCompileCaseInsensitive(`\$\(\s*which\s+`),
		mustCompileCaseInsensitive(`\bsudo\b`),
		mustCompileCaseInsensitive(`\bchmod\s+[0-7]{3,4}\b`),
		mustCompileCaseInsensitive(`\bchown\b`),
		mustCompileCaseInsensitive(`\bpkill\b`),
		mustCompileCaseInsensitive(`\bkillall\b`),
		mustCompileCaseInsensitive(`\bkill\b`),
		mustCompileCaseInsensitive(`\bcurl\b.*\|\s*(sh|bash)`),
		mustCompileCaseInsensitive(`\bwget\b.*\|\s*(sh|bash)`),
		mustCompileCaseInsensitive(`\bnpm\s+install\s+-g\b`),
		mustCompileCaseInsensitive(`\bpip\s+install\s+--user\b`),
		mustCompileCaseInsensitive(`\bapt\s+(install|remove|purge)\b`),
		mustCompileCaseInsensitive(`\byum\s+(install|remove)\b`),
		mustCompileCaseInsensitive(`\bdnf\s+(install|remove)\b`),
		mustCompileCaseInsensitive(`\bdocker\s+run\b`),
		mustCompileCaseInsensitive(`\bdocker\s+exec\b`),
		mustCompileCaseInsensitive(`\bgit\s+push\b`),
		mustCompileCaseInsensitive(`\bgit\s+force\b`),
		mustCompileCaseInsensitive(`\bssh\b.*@`),
		mustCompileCaseInsensitive(`\beval\b`),
		mustCompileCaseInsensitive(`\bsource\s+.*\.sh\b`),
	}

	// windowsDenyPatterns contains PowerShell-specific deny patterns that only
	// apply on Windows, where commands are executed via powershell -Command.
	windowsDenyPatterns = []*regexp.Regexp{
		// [Text.Encoding] used to construct command strings at runtime.
		// Matches [Text.Encoding] and [System.Text.Encoding] variants.
		mustCompileCaseInsensitive(`\[(?:\w+\.)?text\.encoding\]`),
		// PowerShell -EncodedCommand flag (base64-encoded command) and all short forms.
		// Matches: -e, -ec, -enc, -en, -EncodedCommand (all with space prefix)
		mustCompileCaseInsensitive(` -e(?:$|\s)| -ec(?:$|\s)| -enc(?:$|\s)| -en(?:$|\s)| -encodedcommand\b`),
		// .GetString called on byte array to decode commands.
		mustCompileCaseInsensitive(`\.getstring\s*\(\s*\[byte\[\]`),
		// FromBase64String used in command construction chain.
		mustCompileCaseInsensitive(`frombase64string\(`),
		// PowerShell variable holding byte array used in GetString.
		mustCompileCaseInsensitive(`\$[a-zA-Z_]\w*\s*=\s*\[byte\[\]`),
		// Unicode escape sequences that could be used to construct commands.
		// Matches \uXXXX format used to represent characters like i = "i"
		mustCompileCaseInsensitive(`\\u[0-9a-fA-F]{4}`),
	}

	// absolutePathPattern matches absolute file paths in commands (Unix and Windows).
	absolutePathPattern = regexp.MustCompile(`[A-Za-z]:\\[^\\\"']+|/[^\s\"']+`)

	// safePaths are kernel pseudo-devices that are always safe to reference in
	// commands, regardless of workspace restriction. They contain no user data
	// and cannot cause destructive writes.
	safePaths = map[string]bool{
		"/dev/null":    true,
		"/dev/zero":    true,
		"/dev/random":  true,
		"/dev/urandom": true,
		"/dev/stdin":   true,
		"/dev/stdout":  true,
		"/dev/stderr":  true,
	}
)

func NewExecTool(workingDir string, restrict bool, allowPaths ...[]*regexp.Regexp) (*ExecTool, error) {
	return NewExecToolWithConfig(workingDir, restrict, nil, allowPaths...)
}

func NewExecToolWithConfig(
	workingDir string,
	restrict bool,
	cfg *config.Config,
	allowPaths ...[]*regexp.Regexp,
) (*ExecTool, error) {
	denyPatterns := make([]*regexp.Regexp, 0)
	customAllowPatterns := make([]*regexp.Regexp, 0)
	var allowedPathPatterns []*regexp.Regexp
	allowRemote := true
	if len(allowPaths) > 0 {
		allowedPathPatterns = allowPaths[0]
	}

	if cfg != nil {
		execConfig := cfg.Tools.Exec
		enableDenyPatterns := execConfig.EnableDenyPatterns
		allowRemote = execConfig.AllowRemote
		if enableDenyPatterns {
			denyPatterns = append(denyPatterns, defaultDenyPatterns...)
			if runtime.GOOS == "windows" {
				denyPatterns = append(denyPatterns, windowsDenyPatterns...)
			}
			if len(execConfig.CustomDenyPatterns) > 0 {
				logger.InfoCF("tools", "using custom deny patterns", map[string]any{
					"patterns": execConfig.CustomDenyPatterns,
				})
				for _, pattern := range execConfig.CustomDenyPatterns {
					re, err := compileCaseInsensitive(pattern)
					if err != nil {
						return nil, fmt.Errorf("invalid custom deny pattern %q: %w", pattern, err)
					}
					denyPatterns = append(denyPatterns, re)
				}
			}
		} else {
			// If deny patterns are disabled, we won't add any patterns, allowing all commands.
			logger.WarnCF("tools", "deny patterns are disabled, all commands will be allowed", nil)
		}
		for _, pattern := range execConfig.CustomAllowPatterns {
			re, err := compileCaseInsensitive(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid custom allow pattern %q: %w", pattern, err)
			}
			customAllowPatterns = append(customAllowPatterns, re)
		}
	} else {
		denyPatterns = append(denyPatterns, defaultDenyPatterns...)
		if runtime.GOOS == "windows" {
			denyPatterns = append(denyPatterns, windowsDenyPatterns...)
		}
	}

	var timeout time.Duration
	if cfg != nil && cfg.Tools.Exec.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.Tools.Exec.TimeoutSeconds) * time.Second
	}

	return &ExecTool{
		workingDir:          workingDir,
		timeout:             timeout,
		denyPatterns:        denyPatterns,
		allowPatterns:       nil,
		customAllowPatterns: customAllowPatterns,
		allowedPathPatterns: allowedPathPatterns,
		restrictToWorkspace: restrict,
		allowRemote:         allowRemote,
		sessionManager:      getSessionManager(),
	}, nil
}

func (t *ExecTool) Name() string {
	return "exec"
}

func (t *ExecTool) Description() string {
	return `Execute shell commands. Use background=true for long-running commands (returns sessionId). Use pty=true for interactive commands (can combine with background=true). Use poll/read/write/send-keys/kill with sessionId to manage background sessions. Sessions auto-cleanup 30 minutes after process exits; use kill to terminate early. Output buffer limit: 1MB.`
}

//nolint:dupl // Tool parameter schemas intentionally use similar JSON-schema map literals.
func (t *ExecTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"run", "list", "poll", "read", "write", "kill", "send-keys"},
				"default":     "run",
				"description": "Action: run (execute command), list (show sessions), poll (check status), read (get output), write (send input), kill (terminate), send-keys (send keys to PTY)",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "Shell command to execute (required for run)",
			},
			"sessionId": map[string]any{
				"type":        "string",
				"description": "Session ID (required for poll/read/write/kill/send-keys)",
			},
			"keys": map[string]any{
				"type":        "string",
				"description": "Key names for send-keys: up, down, left, right, enter, tab, escape, backspace, ctrl-c, ctrl-d, home, end, pageup, pagedown, f1-f12",
			},
			"data": map[string]any{
				"type":        "string",
				"description": "Data to write to stdin (required for write)",
			},
			"background": map[string]any{
				"type":        "string",
				"description": "Run in background immediately",
			},
			"pty": map[string]any{
				"type":        "string",
				"description": "Run in a pseudo-terminal (PTY) when available",
			},
			"cwd": map[string]any{
				"type":        "string",
				"description": "Working directory for the command",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Timeout in seconds (0 = no timeout)",
			},
		},
		"required": []string{},
	}
}

func (t *ExecTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, _ := args["action"].(string)
	if action == "" {
		action = "run"
	}

	switch action {
	case "run":
		return t.executeRun(ctx, args)
	case "list":
		return t.executeList()
	case "poll":
		return t.executePoll(args)
	case "read":
		return t.executeRead(args)
	case "write":
		return t.executeWrite(args)
	case "kill":
		return t.executeKill(args)
	case "send-keys":
		return t.executeSendKeys(args)
	default:
		return ErrorResult(fmt.Sprintf("unknown action: %s", action))
	}
}

func (t *ExecTool) executeRun(ctx context.Context, args map[string]any) *ToolResult {
	command, ok := args["command"].(string)
	if !ok {
		return ErrorResult("command is required")
	}

	// GHSA-pv8c-p6jf-3fpp: block exec from remote channels (e.g. Telegram webhooks)
	// unless explicitly opted-in via config. Fail-closed: empty channel = blocked.
	if !t.allowRemote {
		channel := ToolChannel(ctx)
		if channel == "" {
			channel, _ = args["__channel"].(string)
		}
		channel = strings.TrimSpace(channel)
		if channel == "" || !constants.IsInternalChannel(channel) {
			return ErrorResult("exec is restricted to internal channels").
				WithErrorKind(ErrInvalidInput)
		}
	}

	getBoolArg := func(key string) bool {
		switch v := args[key].(type) {
		case bool:
			return v
		case string:
			return v == "true"
		}
		return false
	}
	isPty := getBoolArg("pty")
	isBackground := getBoolArg("background")

	if isPty {
		if runtime.GOOS == "windows" {
			return ErrorResult("PTY is not supported on Windows. Use background=true without pty.")
		}
	}

	cwd := t.workingDir
	if wd, ok := args["cwd"].(string); ok && wd != "" {
		if t.restrictToWorkspace && t.workingDir != "" {
			resolvedWD, err := validatePathWithAllowPaths(wd, t.workingDir, true, t.allowedPathPatterns)
			if err != nil {
				// Phase 12.63 (2026-10-26): stamp a typed ErrKind. An empty
				// ErrKind sends the agent recovery layer down the legacy
				// "Tool execution failed:" prefix heuristic, which no
				// safety-guard message matches — recovery never fired and
				// the tool_knowledge lesson for `exec` was never surfaced.
				return ErrorResult("Command blocked by safety guard (" + err.Error() + ")").
					WithErrorKind(ErrInvalidInput)
			}
			cwd = resolvedWD
		} else {
			cwd = wd
		}
	}

	if cwd == "" {
		wd, err := os.Getwd()
		if err == nil {
			cwd = wd
		}
	}

	if guardError := t.guardCommand(command, cwd); guardError != "" {
		// Phase 12.63 (2026-10-26): stamp ErrKind=ErrInvalidInput. The safety
		// guard is a POLICY rejection of the command the LLM chose — retrying
		// the same command cannot help, so this is an invalid-input error,
		// not a transient one. Without a typed kind the recovery layer fell
		// back to the legacy "Tool execution failed:" prefix heuristic, which
		// no guard message matches ⇒ recovery never fired ⇒ the
		// tool_knowledge lesson for `exec` was never surfaced to the LLM.
		return ErrorResult(guardError).WithErrorKind(ErrInvalidInput)
	}

	// Re-resolve symlinks immediately before execution to shrink the TOCTOU window
	// between validation and cmd.Dir assignment.
	if t.restrictToWorkspace && t.workingDir != "" && cwd != t.workingDir {
		resolved, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return ErrorResult(fmt.Sprintf("Command blocked by safety guard (path resolution failed: %v)", err)).
				WithErrorKind(ErrInvalidInput)
		}
		if isAllowedPath(resolved, t.allowedPathPatterns) {
			cwd = resolved
		} else {
			absWorkspace, _ := filepath.Abs(t.workingDir)
			wsResolved, _ := filepath.EvalSymlinks(absWorkspace)
			if wsResolved == "" {
				wsResolved = absWorkspace
			}
			rel, err := filepath.Rel(wsResolved, resolved)
			if err != nil || !filepath.IsLocal(rel) {
				return ErrorResult("Command blocked by safety guard (working directory escaped workspace)").
					WithErrorKind(ErrInvalidInput)
			}
			cwd = resolved
		}
	}

	if isBackground {
		return t.runBackground(ctx, command, cwd, isPty)
	}

	return t.runSync(ctx, command, cwd)
}

func (t *ExecTool) runSync(ctx context.Context, command, cwd string) *ToolResult {
	// timeout == 0 means no timeout
	var cmdCtx context.Context
	var cancel context.CancelFunc
	if t.timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, t.timeout)
	} else {
		cmdCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(cmdCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.CommandContext(cmdCtx, "sh", "-c", command)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	prepareCommandForTermination(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Route shell execution through the shared isolation entry point so exec tool
	// subprocesses receive the same isolation policy as other integrations.
	if err := isolation.Start(cmd); err != nil {
		return ErrorResult(fmt.Sprintf("failed to start command: %v", err))
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorCF("shell", "cmd.Wait goroutine panic recovered",
					map[string]any{
						"panic": fmt.Sprintf("%v", r),
						"stack": string(debug.Stack()),
					})
				done <- fmt.Errorf("panic in cmd.Wait: %v", r)
			}
		}()
		done <- cmd.Wait()
	}()

	var err error
	select {
	case err = <-done:
	case <-cmdCtx.Done():
		_ = terminateProcessTree(cmd)
		select {
		case err = <-done:
		case <-time.After(2 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			err = <-done
		}
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR:\n" + stderr.String()
	}

	if err != nil {
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			msg := fmt.Sprintf("Command timed out after %v", t.timeout)
			if output != "" {
				msg += "\n\nPartial output before timeout:\n" + output
			}
			return &ToolResult{
				ForLLM:  msg,
				ForUser: msg,
				IsError: true,
				Err:     fmt.Errorf("command timeout: %w", err),
			}
		}

		// Extract detailed exit information
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode := exitErr.ExitCode()
			output += fmt.Sprintf("\n\n[Command exited with code %d]", exitCode)

			// Add signal information if killed by signal (Unix)
			if exitCode == -1 {
				output += " (killed by signal)"
			}
		} else {
			output += fmt.Sprintf("\n\n[Command failed: %v]", err)
		}
	}

	if output == "" {
		output = "(no output)"
	}

	maxLen := 10000
	if len(output) > maxLen {
		output = output[:maxLen] + fmt.Sprintf("\n... (truncated, %d more chars)", len(output)-maxLen)
	}

	if err != nil {
		return &ToolResult{
			ForLLM:  output,
			ForUser: output,
			IsError: true,
		}
	}

	return &ToolResult{
		ForLLM:  output,
		ForUser: output,
		IsError: false,
	}
}

func (t *ExecTool) runBackground(ctx context.Context, command, cwd string, ptyEnabled bool) *ToolResult {
	sessionID := generateSessionID()
	session := &ProcessSession{
		ID:         sessionID,
		Command:    command,
		PTY:        ptyEnabled,
		Background: true,
		StartTime:  time.Now().Unix(),
		Status:     "running",
		ptyKeyMode: PtyKeyModeCSI,
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	prepareCommandForTermination(cmd)

	var stdoutReader io.ReadCloser
	var stderrReader io.ReadCloser
	var stdinWriter io.WriteCloser

	if ptyEnabled {
		ptmx, tty, err := pty.Open()
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to create PTY: %v", err))
		}

		cmd.Stdin = tty
		cmd.Stdout = tty
		cmd.Stderr = tty

		// For PTY, we need Setsid to create a new session.
		// Note: Setsid and Setpgid conflict, so we must replace SysProcAttr entirely.
		setSysProcAttrForPty(cmd)

		session.ptyMaster = ptmx
	} else {
		var err error
		stdoutReader, err = cmd.StdoutPipe()
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to create stdout pipe: %v", err))
		}
		stderrReader, err = cmd.StderrPipe()
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to create stderr pipe: %v", err))
		}
		stdinWriter, err = cmd.StdinPipe()
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to create stdin pipe: %v", err))
		}
		session.stdoutPipe = io.MultiReader(stdoutReader, stderrReader)
		session.stdinWriter = stdinWriter
	}

	// Background sessions use the same startup path so isolation stays consistent
	// with synchronous exec runs.
	if err := isolation.Start(cmd); err != nil {
		if session.ptyMaster != nil {
			_ = session.ptyMaster.Close()
		}
		return ErrorResult(fmt.Sprintf("failed to start command: %v", err))
	}

	session.PID = cmd.Process.Pid
	t.sessionManager.Add(session)

	session.outputBuffer = &bytes.Buffer{}

	// PTY mode: read from ptyMaster and wait for process
	// Note: On Linux, closing ptyMaster doesn't interrupt blocking Read() calls,
	// so we need cmd.Wait() in a separate goroutine to detect process exit.
	if session.PTY && session.ptyMaster != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.ErrorCF("shell", "PTY cmd.Wait goroutine panic recovered",
						map[string]any{
							"panic": fmt.Sprintf("%v", r),
							"stack": string(debug.Stack()),
						})
					session.mu.Lock()
					session.Status = "error"
					session.mu.Unlock()
				}
			}()
			cmd.Wait() // Wait for process to exit
			session.mu.Lock()
			if cmd.ProcessState != nil {
				session.ExitCode = cmd.ProcessState.ExitCode()
			}
			session.Status = "done"
			session.mu.Unlock()
		}()

		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.ErrorCF("shell", "PTY read goroutine panic recovered",
						map[string]any{
							"panic": fmt.Sprintf("%v", r),
							"stack": string(debug.Stack()),
						})
				}
			}()
			buf := make([]byte, 4096)
			for {
				n, err := session.ptyMaster.Read(buf)
				if n > 0 {
					raw := string(buf[:n])
					if mode := detectPtyKeyMode(raw); mode != PtyKeyModeNotFound && mode != session.GetPtyKeyMode() {
						session.SetPtyKeyMode(mode)
					}

					session.mu.Lock()
					if session.outputBuffer.Len() >= maxOutputBufferSize {
						if !session.outputTruncated {
							session.outputBuffer.WriteString(outputTruncateMarker)
							session.outputTruncated = true
						}
					} else {
						session.outputBuffer.Write(buf[:n])
					}
					session.mu.Unlock()
				}
				if err != nil {
					break
				}
			}
		}()
	} else {
		// Non-PTY mode: single goroutine reads pipes.
		// When Read() returns EOF (pipe closed), we break.
		// When process exits, OS closes pipe write end → Read() returns EOF → we exit.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.ErrorCF("shell", "pipe read goroutine panic recovered",
						map[string]any{
							"panic": fmt.Sprintf("%v", r),
							"stack": string(debug.Stack()),
						})
				}
			}()
			buf := make([]byte, 4096)

			// Read stdout
			for {
				n, err := stdoutReader.Read(buf)
				if n > 0 {
					session.mu.Lock()
					if session.outputBuffer.Len() >= maxOutputBufferSize {
						if !session.outputTruncated {
							session.outputBuffer.WriteString(outputTruncateMarker)
							session.outputTruncated = true
						}
					} else {
						session.outputBuffer.Write(buf[:n])
					}
					session.mu.Unlock()
				}
				if err != nil {
					break
				}
			}

			// Read stderr
			for {
				n, err := stderrReader.Read(buf)
				if n > 0 {
					session.mu.Lock()
					if session.outputBuffer.Len() >= maxOutputBufferSize {
						if !session.outputTruncated {
							session.outputBuffer.WriteString(outputTruncateMarker)
							session.outputTruncated = true
						}
					} else {
						session.outputBuffer.Write(buf[:n])
					}
					session.mu.Unlock()
				}
				if err != nil {
					break
				}
			}

			// All pipes closed, get exit status
			if stdinWriter != nil {
				_ = stdinWriter.Close()
			}
			cmd.Wait()

			session.mu.Lock()
			if cmd.ProcessState != nil {
				session.ExitCode = cmd.ProcessState.ExitCode()
			}
			session.Status = "done"
			session.mu.Unlock()
		}()
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    "running",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("Session %s started", sessionID),
		IsError: false,
	}
}

func (t *ExecTool) executeList() *ToolResult {
	sessions := t.sessionManager.List()
	resp := ExecResponse{
		Sessions: sessions,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("%d active sessions", len(sessions)),
		IsError: false,
	}
}

func (t *ExecTool) executePoll(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    session.GetStatus(),
		ExitCode:  session.GetExitCode(),
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		IsError: false,
	}
}

func (t *ExecTool) executeRead(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	output := session.Read()

	resp := ExecResponse{
		SessionID: sessionID,
		Output:    output,
		Status:    session.GetStatus(),
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		IsError: false,
	}
}

func (t *ExecTool) executeWrite(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	data, ok := args["data"].(string)
	if !ok {
		return ErrorResult("data is required")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	if session.IsDone() {
		return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
	}

	if err = session.Write(data); err != nil {
		if errors.Is(err, ErrSessionDone) {
			return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
		}
		return ErrorResult(fmt.Sprintf("failed to write to session: %v", err))
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    session.GetStatus(),
	}
	respData, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(respData),
		IsError: false,
	}
}

func (t *ExecTool) executeKill(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	if session.IsDone() {
		return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
	}

	if err = session.Kill(); err != nil {
		return ErrorResult(fmt.Sprintf("failed to kill session: %v", err))
	}

	t.sessionManager.Remove(sessionID)

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    "done",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("Session %s killed", sessionID),
		IsError: false,
	}
}

// keyMap maps key names to their escape sequences.
var keyMap = map[string]string{
	"enter":     "\r",
	"return":    "\r",
	"tab":       "\t",
	"escape":    "\x1b",
	"esc":       "\x1b",
	"space":     " ",
	"backspace": "\x7f",
	"bspace":    "\x7f",
	"up":        "\x1b[A",
	"down":      "\x1b[B",
	"right":     "\x1b[C",
	"left":      "\x1b[D",
	"home":      "\x1b[1~",
	"end":       "\x1b[4~",
	"pageup":    "\x1b[5~",
	"pagedown":  "\x1b[6~",
	"pgup":      "\x1b[5~",
	"pgdn":      "\x1b[6~",
	"insert":    "\x1b[2~",
	"ic":        "\x1b[2~",
	"delete":    "\x1b[3~",
	"del":       "\x1b[3~",
	"dc":        "\x1b[3~",
	"btab":      "\x1b[Z",
	"f1":        "\x1bOP",
	"f2":        "\x1bOQ",
	"f3":        "\x1bOR",
	"f4":        "\x1bOS",
	"f5":        "\x1b[15~",
	"f6":        "\x1b[17~",
	"f7":        "\x1b[18~",
	"f8":        "\x1b[19~",
	"f9":        "\x1b[20~",
	"f10":       "\x1b[21~",
	"f11":       "\x1b[23~",
	"f12":       "\x1b[24~",
}

// ss3KeysMap maps key names to SS3 escape sequences
var ss3KeysMap = map[string]string{
	"up":    "\x1bOA",
	"down":  "\x1bOB",
	"right": "\x1bOC",
	"left":  "\x1bOD",
	"home":  "\x1bOH",
	"end":   "\x1bOF",
}

func detectPtyKeyMode(raw string) PtyKeyMode {
	const SMKX = "\x1b[?1h"
	const RMKX = "\x1b[?1l"

	lastSmkx := strings.LastIndex(raw, SMKX)
	lastRmkx := strings.LastIndex(raw, RMKX)

	if lastSmkx == -1 && lastRmkx == -1 {
		return PtyKeyModeNotFound
	}

	if lastSmkx > lastRmkx {
		return PtyKeyModeSS3
	}
	return PtyKeyModeCSI
}

// encodeKeyToken encodes a single key token into its escape sequence.
// Supports:
//   - Named keys: "enter", "tab", "up", "ctrl-c", "alt-x", etc.
//   - Ctrl modifier: "ctrl-c" or "c-c" (sends Ctrl+char)
//   - Alt modifier: "alt-x" or "m-x" (sends ESC+char)
func encodeKeyToken(token string, ptyKeyMode PtyKeyMode) (string, error) {
	token = strings.ToLower(strings.TrimSpace(token))
	if token == "" {
		return "", nil
	}

	// Handle ctrl-X format (c-x)
	if strings.HasPrefix(token, "c-") {
		char := token[2]
		if char >= 'a' && char <= 'z' {
			return string(rune(char) & 0x1f), nil // ctrl-a through ctrl-z
		}
		return "", fmt.Errorf("invalid ctrl key: %s", token)
	}

	// Handle ctrl-X format (ctrl-x)
	if strings.HasPrefix(token, "ctrl-") {
		char := token[5]
		if char >= 'a' && char <= 'z' {
			return string(rune(char) & 0x1f), nil
		}
		return "", fmt.Errorf("invalid ctrl key: %s", token)
	}

	// Handle alt-X format (m-x or alt-x)
	if strings.HasPrefix(token, "m-") || strings.HasPrefix(token, "alt-") {
		var char string
		if strings.HasPrefix(token, "m-") {
			char = token[2:]
		} else {
			char = token[4:]
		}
		if len(char) == 1 {
			return "\x1b" + char, nil
		}
		return "", fmt.Errorf("invalid alt key: %s", token)
	}

	// Handle shift modifier for special keys (shift-up, shift-down, etc.)
	if strings.HasPrefix(token, "s-") || strings.HasPrefix(token, "shift-") {
		var key string
		if strings.HasPrefix(token, "s-") {
			key = token[2:]
		} else {
			key = token[6:]
		}
		// Apply shift modifier: for single-char keys, return uppercase
		if seq, ok := keyMap[key]; ok {
			// For escape sequences, we can't easily add shift
			// For single-char keys (letters), return uppercase
			if len(seq) == 1 {
				return strings.ToUpper(seq), nil
			}
			return seq, nil
		}
		return "", fmt.Errorf("unknown key with shift: %s", key)
	}

	if ptyKeyMode == PtyKeyModeSS3 {
		if seq, ok := ss3KeysMap[token]; ok {
			return seq, nil
		}
	}

	if seq, ok := keyMap[token]; ok {
		return seq, nil
	}

	return "", fmt.Errorf("unknown key: %s (use write action for text input)", token)
}

// encodeKeySequence encodes a slice of key tokens into a single string.
func encodeKeySequence(tokens []string, ptyKeyMode PtyKeyMode) (string, error) {
	var result string
	for _, token := range tokens {
		seq, err := encodeKeyToken(token, ptyKeyMode)
		if err != nil {
			return "", err
		}
		result += seq
	}
	return result, nil
}

func (t *ExecTool) executeSendKeys(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	keysStr, ok := args["keys"].(string)
	if !ok {
		return ErrorResult("keys must be a string")
	}

	if keysStr == "" {
		return ErrorResult("keys cannot be empty")
	}

	// Parse comma-separated key names
	keyNames := strings.Split(keysStr, ",")
	var keys []string
	for _, k := range keyNames {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}

	if len(keys) == 0 {
		return ErrorResult("keys cannot be empty")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	ptyKeyMode := session.GetPtyKeyMode()

	data, err := encodeKeySequence(keys, ptyKeyMode)
	if err != nil {
		return ErrorResult(fmt.Sprintf("invalid key: %v", err))
	}

	if session.IsDone() {
		return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
	}

	if err = session.Write(data); err != nil {
		if errors.Is(err, ErrSessionDone) {
			return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
		}
		return ErrorResult(fmt.Sprintf("failed to send keys: %v", err))
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    "running",
		Output:    fmt.Sprintf("Sent keys: %v", keys),
	}
	respData, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(respData),
		IsError: false,
	}
}

// expandPowerShellEnvVars expands environment variable syntax used by both
// PowerShell ($env:VAR) and CMD (%VAR%) to their actual values.
func expandPowerShellEnvVars(cmd string) string {
	// Handle PowerShell style: $env:VAR and ${env:VAR}
	rePs := regexp.MustCompile(`\$\{?env:(\w+)\}?`)
	cmd = rePs.ReplaceAllStringFunc(cmd, func(match string) string {
		varName := rePs.FindStringSubmatch(match)[1]
		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match
	})

	// Handle CMD style: %VAR%
	reCmd := regexp.MustCompile(`%([^%]+)%`)
	return reCmd.ReplaceAllStringFunc(cmd, func(match string) string {
		varName := reCmd.FindStringSubmatch(match)[1]
		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match
	})
}

func (t *ExecTool) guardCommand(command, cwd string) string {
	cmd := strings.TrimSpace(command)

	// The patterns are matched against the ORIGINAL command, not a
	// lowercased copy. The patterns themselves are compiled
	// case-insensitively (see compileCaseInsensitive), so the guard
	// still matches `SUDO` and `sudo` alike — but the subject keeps its
	// real casing, which the other checks in this function (absolute
	// paths, path traversal) depend on.
	//
	// Matching a lowercased copy was the root cause of the dead-rule
	// bug: a pattern with an UPPERCASE literal could never fire.

	// Custom allow patterns exempt a command from deny checks.
	explicitlyAllowed := false
	for _, pattern := range t.customAllowPatterns {
		if pattern.MatchString(cmd) {
			explicitlyAllowed = true
			break
		}
	}

	if !explicitlyAllowed {
		for _, pattern := range t.denyPatterns {
			if pattern.MatchString(cmd) {
				return "Command blocked by safety guard (dangerous pattern detected)"
			}
		}
	}

	// The heredoc check runs AFTER the allowlist on purpose.
	//
	// A heredoc rule living in denyPatterns would be skipped entirely
	// for any command that matches custom_allow_patterns (the deny loop
	// only runs when `!explicitlyAllowed`), so `git apply <<EOF` would
	// pass while `cat <<EOF` is blocked. As a separate check it cannot
	// be bypassed that way.
	//
	// Cost of that decision, stated plainly: this rule is not switched
	// off by `tools.exec.enable_deny_patterns: false`. It is a
	// construct-level rule, not a pattern.
	if reason := detectDangerousHeredoc(cmd); reason != "" {
		return "Command blocked by safety guard (" + reason + ")"
	}

	if len(t.allowPatterns) > 0 {
		allowed := false
		for _, pattern := range t.allowPatterns {
			if pattern.MatchString(cmd) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "Command blocked by safety guard (not in allowlist)"
		}
	}

	if t.restrictToWorkspace {
		// Block path traversal patterns including .../.../ variants
		if regexp.MustCompile(`\.\.(?:[\\/]\.\.)*[\\/]`).MatchString(cmd) {
			return "Command blocked by safety guard (path traversal detected)"
		}

		cwdPath, err := filepath.Abs(cwd)
		if err != nil {
			return ""
		}

		// Web URL schemes whose path components (starting with //) should be exempt
		// from workspace sandbox checks. file: is intentionally excluded so that
		// file:// URIs are still validated against the workspace boundary.
		webSchemes := []string{"http:", "https:", "ftp:", "ftps:", "sftp:", "ssh:", "git:"}

		// On Windows, expand ~ and PowerShell environment variables ($env:VAR) before path checking
		if runtime.GOOS == "windows" {
			// Expand PowerShell environment variables ($env:VAR and ${env:VAR})
			cmd = expandPowerShellEnvVars(cmd)
			// Also expand ~ for completeness
			if home, err := os.UserHomeDir(); err == nil {
				cmd = strings.ReplaceAll(cmd, "~", filepath.FromSlash(home))
			}
		}

		matchIndices := absolutePathPattern.FindAllStringIndex(cmd, -1)

		for _, loc := range matchIndices {
			raw := cmd[loc[0]:loc[1]]

			// Skip URL path components that look like they're from web URLs.
			// When a URL like "https://github.com" is parsed, the regex captures
			// "//github.com" as a match (the path portion after "https:").
			// Use the exact match position (loc[0]) so that duplicate //path substrings
			// in the same command are each evaluated at their own position.
			if strings.HasPrefix(raw, "//") && loc[0] > 0 {
				before := cmd[:loc[0]]
				isWebURL := false

				for _, scheme := range webSchemes {
					if strings.HasSuffix(before, scheme) {
						isWebURL = true
						break
					}
				}

				if isWebURL {
					continue
				}
			}

			// Skip scheme-less URL paths like "wttr.in/Beijing".
			// When a /path is immediately preceded by a token that looks
			// like a domain name and that token does NOT exist as a local
			// filesystem entry, treat the path as part of a URL and skip
			// workspace sandbox validation.
			//
			// The local-path-exists guard prevents symlink bypass: if
			// "foo.bar" exists as a local symlink or directory, the path
			// still undergoes full workspace validation (see #2965).
			if loc[0] > 0 && raw[0] == '/' {
				// Find the token immediately before the "/".
				j := loc[0] - 1
				for j >= 0 && !isShellTokenBoundary(cmd[j]) {
					j--
				}
				token := cmd[j+1 : loc[0]]
				if looksLikeDomain(token) && !localPathExists(cwd, token) {
					continue
				}
			}

			p, err := commandPathAbs(commandPathTextFromMatch(cmd, loc[0], loc[1]), cwdPath)
			if err != nil {
				continue
			}

			// Windows-specific: normalize paths to block ADS and extended-length paths
			if runtime.GOOS == "windows" {
				// Strip \\?\ prefix (extended-length path)
				p = strings.TrimPrefix(p, `\\?\`)
				// Strip NTFS alternate data streams (only if colon is not at position 1 = drive letter)
				if idx := strings.Index(p, ":"); idx > 1 {
					p = p[:idx]
				}
			}

			// Check symlinks and junctions
			resolved, err := filepath.EvalSymlinks(p)
			if err == nil {
				p = resolved
			}

			if safePaths[p] {
				continue
			}
			if isAllowedPath(p, t.allowedPathPatterns) {
				continue
			}

			rel, err := filepath.Rel(cwdPath, p)
			if err != nil {
				continue
			}

			if strings.HasPrefix(rel, "..") {
				return "Command blocked by safety guard (path outside working dir)"
			}
		}
	}

	return ""
}

func commandPathAbs(pathText, cwdPath string) (string, error) {
	if filepath.IsAbs(pathText) {
		return filepath.Abs(pathText)
	}
	return filepath.Abs(filepath.Join(cwdPath, pathText))
}

func commandPathTextFromMatch(cmd string, start, end int) string {
	raw := cmd[start:end]
	if !strings.HasPrefix(raw, "/") || isUnixAbsolutePathMatchStart(cmd, start) {
		return raw
	}

	tokenStart, tokenEnd := shellTokenBounds(cmd, start)
	prefix := cmd[tokenStart:start]
	// For --flag=rel/path, validate the value. For ambiguous attached option
	// forms like -isystem/path, keep the slash-starting path conservative.
	if eq := strings.IndexByte(prefix, '='); eq >= 0 {
		return cmd[tokenStart+eq+1 : tokenEnd]
	}
	if strings.HasPrefix(prefix, "-") {
		return raw
	}
	return cmd[tokenStart:tokenEnd]
}

func shellTokenBounds(cmd string, idx int) (int, int) {
	start := idx
	for start > 0 && !isShellTokenBoundary(cmd[start-1]) {
		start--
	}
	end := idx
	for end < len(cmd) && !isShellTokenBoundary(cmd[end]) {
		end++
	}
	return start, end
}

// isUnixAbsolutePathMatchStart returns true when a regex match beginning with
// "/" is actually an absolute path token, not the separator inside a relative
// path such as "skills/foo.py".
func isUnixAbsolutePathMatchStart(cmd string, idx int) bool {
	if idx <= 0 {
		return true
	}

	prev := cmd[idx-1]
	if isShellTokenBoundary(prev) || prev == '=' || prev == ',' || prev == '(' || prev == '[' || prev == '{' {
		return true
	}

	j := idx - 1
	for j >= 0 && !isShellTokenBoundary(cmd[j]) {
		j--
	}
	prefix := cmd[j+1 : idx]

	return strings.HasPrefix(prefix, "-") && !strings.Contains(prefix, "=")
}

// isShellTokenBoundary returns true when b is a byte that separates
// tokens in a shell command (space, tab, colon, semicolon, pipe, etc.).
func isShellTokenBoundary(b byte) bool {
	switch b {
	case ' ', '\t', ':', ';', '|', '&', '<', '>', '\'', '"', '`', '\n', '\r':
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Heredoc detection
//
// A heredoc (`cat <<EOF`) whose delimiter is NOT quoted has its body
// subjected to parameter expansion, command substitution and arithmetic
// expansion by the shell. That makes the body an injection surface even
// when the body itself looks harmless. A QUOTED delimiter (`cat <<'EOF'`)
// suppresses that expansion.
//
// This rule used to be the deny pattern `<<\s*EOF`. It was DEAD since
// birth (commit 01d694b9, 2026-02-18) because the guard matched patterns
// against the LOWERCASED command while the pattern requires an UPPERCASE
// literal. It is implemented in code instead, for three reasons:
//
//  1. A pattern inside denyPatterns is bypassable: guardCommand evaluates
//     the custom allowlist FIRST and only runs the deny loop when the
//     command was not allow-listed. `git apply <<EOF` would pass while
//     `cat <<EOF` is blocked.
//  2. A pattern cannot be quote-aware. Making `<<\s*EOF` case-insensitive
//     makes it fire on `<<EOF` text that sits inside a quoted string.
//     Measured on the real exec corpus (3098 commands): the pattern
//     matches 3 commands and ALL 3 are inside a double-quoted region
//     (an `echo "=== mentioning <<EOF ==="` banner).
//  3. A pattern is delimiter-specific: it only knows the literal `EOF`.
//     The scanner handles `<<PYEOF`, `<<SCRIPT`, `<<'EOF'`, `<<"EOF"`.
//
// The scanner is quote-aware, so `git log -S '<<\s*EOF'` (a quoted regex)
// and `f.read(1<<20)` (a bit-shift inside quoted code) are not affected.
//
// The byte constants below are numeric on purpose. The scanner must not
// depend on escape sequences in this file's literals.

// heredocOperator describes one `<<` or `<<-` operator found OUTSIDE any
// quoted region.
type heredocOperator struct {
	index     int    // index of the first '<'
	delimiter string // the delimiter word, without its quotes
	quoted    bool   // true when the delimiter was quoted
	next      int    // index just past the delimiter
}

// heredocByte* are the byte values the scanner compares against.
const (
	heredocByteNewline     byte = 10
	heredocByteCR          byte = 13
	heredocByteTab         byte = 9
	heredocByteSpace       byte = 32
	heredocByteBackslash   byte = 92
	heredocByteSingleQuote byte = 39
	heredocByteDoubleQuote byte = 34
	heredocByteBacktick    byte = 96
	heredocBytePipe        byte = 124
	heredocByteLess        byte = 60
	heredocByteDash        byte = 45
)

// shellInterpreters are the programs that EXECUTE their standard input as
// a script. Feeding a heredoc into one of them is the strongest signal of
// intent, and it is dangerous even when the delimiter is quoted (quoting
// stops expansion, not execution).
//
// `python3` and friends are deliberately absent: a heredoc piped into a
// Python interpreter is a normal, safe idiom (7 real occurrences in the
// measured corpus).
var shellInterpreters = map[string]bool{
	"sh":      true,
	"bash":    true,
	"zsh":     true,
	"dash":    true,
	"ksh":     true,
	"csh":     true,
	"tcsh":    true,
	"ash":     true,
	"fish":    true,
	"busybox": true,
}

// detectDangerousHeredoc reports why cmd must be refused, or an empty
// string when the command contains no dangerous heredoc.
//
// It returns one of:
//   - "unquoted heredoc delimiter" — the body is subject to shell expansion
//   - "heredoc fed to a shell interpreter" — `bash <<EOF`
//   - "heredoc piped to a shell interpreter" — `cat <<EOF | sh`
func detectDangerousHeredoc(cmd string) string {
	for _, op := range scanHeredocOperators(cmd) {
		if isShellInterpreter(heredocWordBefore(cmd, op.index)) {
			return "heredoc fed to a shell interpreter"
		}
		if sink := heredocPipeSink(cmd, op); sink != "" {
			return "heredoc piped to a shell interpreter"
		}
		if !op.quoted {
			return "unquoted heredoc delimiter"
		}
	}
	return ""
}

// scanHeredocOperators returns every heredoc operator in cmd that is
// outside a quoted region and sits in a position where a shell would read
// it as a redirection.
//
// The scan is quote-aware: single quotes, double quotes, backticks and
// backslash escapes are skipped, so `<<` inside a string is text, not an
// operator.
func scanHeredocOperators(cmd string) []heredocOperator {
	var out []heredocOperator
	n := len(cmd)
	i := 0
	for i < n {
		switch cmd[i] {
		case heredocByteBackslash:
			i += 2
			continue
		case heredocByteSingleQuote:
			i = skipHeredocQuoted(cmd, i, heredocByteSingleQuote)
			continue
		case heredocByteDoubleQuote:
			i = skipHeredocQuoted(cmd, i, heredocByteDoubleQuote)
			continue
		case heredocByteBacktick:
			i = skipHeredocQuoted(cmd, i, heredocByteBacktick)
			continue
		}
		if cmd[i] == heredocByteLess && i+1 < n && cmd[i+1] == heredocByteLess {
			// `<<<` is a here-STRING: a distinct operator with no
			// delimiter and no body. It is deliberately NOT a heredoc.
			if i+2 < n && cmd[i+2] == heredocByteLess {
				i += 3
				continue
			}
			if heredocOperatorPosition(cmd, i) {
				op := parseHeredocOperator(cmd, i)
				if op.delimiter != "" {
					out = append(out, op)
					i = op.next
					continue
				}
			}
			i += 2
			continue
		}
		i++
	}
	return out
}

// skipHeredocQuoted returns the index just past the quoted region that
// starts at cmd[start]. A backslash escapes the next byte inside double
// quotes and backticks, but NOT inside single quotes.
func skipHeredocQuoted(cmd string, start int, quote byte) int {
	n := len(cmd)
	j := start + 1
	for j < n {
		if quote != heredocByteSingleQuote && cmd[j] == heredocByteBackslash {
			j += 2
			continue
		}
		if cmd[j] == quote {
			return j + 1
		}
		j++
	}
	return n
}

// heredocOperatorPosition reports whether the `<<` at index i sits where
// a shell reads a redirection.
//
// A `<<` glued to the end of a word (`cat<<EOF`) is a real heredoc in
// bash, so it is accepted — EXCEPT when the glued word is a number, which
// is the bit-shift shape `1<<20`. The corpus contains no glued operator at
// all, so this branch never fires on real traffic; it exists so that
// deleting one space cannot bypass the rule.
func heredocOperatorPosition(cmd string, i int) bool {
	if i == 0 {
		return true
	}
	prev := cmd[i-1]
	if isShellTokenBoundary(prev) {
		return true
	}
	if !isHeredocWordByte(prev) {
		return false
	}
	j := i - 1
	for j >= 0 && isHeredocWordByte(cmd[j]) {
		j--
	}
	glued := cmd[j+1 : i]
	for k := 0; k < len(glued); k++ {
		if glued[k] < '0' || glued[k] > '9' {
			return true
		}
	}
	return false
}

// parseHeredocOperator reads the operator at index i and its delimiter.
func parseHeredocOperator(cmd string, i int) heredocOperator {
	n := len(cmd)
	op := heredocOperator{index: i}

	j := i + 2
	if j < n && cmd[j] == heredocByteDash {
		// `<<-` strips leading tabs from the body. Same danger.
		j++
	}
	for j < n && (cmd[j] == heredocByteSpace || cmd[j] == heredocByteTab) {
		j++
	}
	if j >= n {
		return op
	}

	switch cmd[j] {
	case heredocByteSingleQuote:
		op.quoted = true
		op.delimiter, op.next = heredocQuotedContent(cmd, j, heredocByteSingleQuote)
	case heredocByteDoubleQuote:
		op.quoted = true
		op.delimiter, op.next = heredocQuotedContent(cmd, j, heredocByteDoubleQuote)
	case heredocByteBackslash:
		// `<<\EOF` quotes the delimiter.
		op.quoted = true
		j++
		start := j
		for j < n && !isHeredocDelimiterEnd(cmd[j]) {
			j++
		}
		op.delimiter = cmd[start:j]
		op.next = j
	default:
		start := j
		for j < n && !isHeredocDelimiterEnd(cmd[j]) {
			j++
		}
		op.delimiter = cmd[start:j]
		op.next = j
	}
	return op
}

// heredocQuotedContent returns the content of the quoted region that
// starts at cmd[start] and the index just past it.
func heredocQuotedContent(cmd string, start int, quote byte) (string, int) {
	end := skipHeredocQuoted(cmd, start, quote)
	contentEnd := end
	if contentEnd > 0 && contentEnd <= len(cmd) && cmd[contentEnd-1] == quote {
		contentEnd--
	}
	if contentEnd < start+1 {
		contentEnd = start + 1
	}
	return cmd[start+1 : contentEnd], end
}

// heredocPipeSink returns the shell interpreter that the heredoc is piped
// into, or an empty string.
//
// The sink is looked for on the operator's OWN line: that is where a pipe
// can appear. A heredoc body starts on the following line, so a pipe on a
// later line belongs to a different command.
//
// Looking AFTER the operator is deliberate. The earlier measurement that
// reported "shell-sink = 0" was a blind spot of a detector that only
// inspected the text BEFORE the `<<`, which cannot see `cat <<EOF | sh`.
func heredocPipeSink(cmd string, op heredocOperator) string {
	n := len(cmd)
	j := op.next
	for j < n && cmd[j] != heredocByteNewline && cmd[j] != heredocByteCR {
		if cmd[j] == heredocBytePipe {
			if j+1 < n && cmd[j+1] == heredocBytePipe {
				// `||` is a conditional, not a pipe.
				j += 2
				continue
			}
			word := heredocWordAfter(cmd, j+1)
			if isShellInterpreter(word) {
				return word
			}
		}
		j++
	}
	return ""
}

// isShellInterpreter reports whether word names a shell that executes its
// standard input as a script.
func isShellInterpreter(word string) bool {
	if word == "" {
		return false
	}
	if idx := strings.LastIndexByte(word, '/'); idx >= 0 {
		word = word[idx+1:]
	}
	return shellInterpreters[word]
}

// heredocWordBefore returns the last shell word that ends just before
// index i.
func heredocWordBefore(cmd string, i int) string {
	j := i - 1
	for j >= 0 && isHeredocSpace(cmd[j]) {
		j--
	}
	if j < 0 {
		return ""
	}
	end := j + 1
	for j >= 0 && !isHeredocSpace(cmd[j]) && !isHeredocMetaByte(cmd[j]) {
		j--
	}
	return cmd[j+1 : end]
}

// heredocWordAfter returns the first shell word that starts at or after
// index i.
func heredocWordAfter(cmd string, i int) string {
	n := len(cmd)
	j := i
	for j < n && isHeredocSpace(cmd[j]) {
		j++
	}
	start := j
	for j < n && !isHeredocSpace(cmd[j]) && !isHeredocMetaByte(cmd[j]) {
		j++
	}
	return cmd[start:j]
}

// isHeredocSpace reports whether b is a whitespace byte.
func isHeredocSpace(b byte) bool {
	switch b {
	case heredocByteSpace, heredocByteTab, heredocByteNewline, heredocByteCR:
		return true
	}
	return false
}

// isHeredocMetaByte reports whether b is a shell metacharacter that ends
// a word.
func isHeredocMetaByte(b byte) bool {
	switch b {
	case '|', '&', ';', '<', '>', '(', ')':
		return true
	}
	return false
}

// isHeredocDelimiterEnd reports whether b ends a delimiter word.
func isHeredocDelimiterEnd(b byte) bool {
	if isHeredocSpace(b) {
		return true
	}
	return isHeredocMetaByte(b)
}

// isHeredocWordByte reports whether b can be part of a shell word.
func isHeredocWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '_' || b == '.' || b == '/' || b == '-':
		return true
	}
	return false
}

// looksLikeDomain returns true when s looks like a DNS domain name:
// it contains at least one dot, starts with an alphanumeric character,
// and does not end with a common file extension.
func looksLikeDomain(s string) bool {
	if len(s) < 3 || !strings.ContainsRune(s, '.') {
		return false
	}
	first := s[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return false
	}
	// Exclude tokens ending with common file/programming extensions,
	// e.g. "script.py", "main.go", "app.exe".
	if idx := strings.LastIndexByte(s, '.'); idx >= 0 {
		ext := strings.ToLower(s[idx+1:])
		if commonFileExtension(ext) {
			return false
		}
	}
	return true
}

// commonFileExtension returns true when ext is a file extension that
// strongly indicates a local file rather than a domain TLD.
func commonFileExtension(ext string) bool {
	switch ext {
	case "py", "js", "ts", "tsx", "jsx", "go", "rs", "rb", "php",
		"java", "c", "cpp", "h", "hpp", "cs", "swift", "kt", "scala",
		"sh", "bash", "zsh", "fish", "ps1", "bat", "cmd",
		"txt", "md", "rst", "log", "json", "yaml", "yml", "toml",
		"xml", "html", "css", "scss", "ini", "cfg", "conf", "env",
		"exe", "dll", "so", "dylib", "lib", "a", "o", "obj",
		"zip", "tar", "gz", "bz2", "xz", "7z", "rar",
		"png", "jpg", "jpeg", "gif", "svg", "ico", "bmp", "webp",
		"mp3", "mp4", "wav", "avi", "mov", "mkv", "flac",
		"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx",
		"pub", "pem", "key", "crt", "cer", "p12", "pfx",
		"bak", "tmp", "swp", "lock",
		"ttf", "otf", "woff", "woff2", "eot",
		"deb", "rpm", "apk", "msi", "dmg",
		"sql", "sqlite", "db":
		return true
	}
	return false
}

// localPathExists returns true when the given token resolves to an
// existing filesystem entry relative to cwd.
func localPathExists(cwd, token string) bool {
	info, err := os.Lstat(filepath.Join(cwd, token))
	return err == nil && info != nil
}

func (t *ExecTool) SetTimeout(timeout time.Duration) {
	t.timeout = timeout
}

func (t *ExecTool) SetRestrictToWorkspace(restrict bool) {
	t.restrictToWorkspace = restrict
}

func (t *ExecTool) SetAllowPatterns(patterns []string) error {
	t.allowPatterns = make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := compileCaseInsensitive(p)
		if err != nil {
			return fmt.Errorf("invalid allow pattern %q: %w", p, err)
		}
		t.allowPatterns = append(t.allowPatterns, re)
	}
	return nil
}
