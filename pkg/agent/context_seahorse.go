//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
	"github.com/sipeed/picoclaw/pkg/seahorse"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// syncStateEntry tracks the known file size and message count for a session's JSONL file.
// Persisted in sync_state.json to avoid re-scanning unchanged sessions on startup.
type syncStateEntry struct {
	FileSize     int64  `json:"file_size"`
	MessageCount int    `json:"message_count"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// seahorseContextManager adapts seahorse.Engine to agent.ContextManager.
type seahorseContextManager struct {
	engine        *seahorse.Engine
	sessions      session.SessionStore // for startup bootstrap
	agent         *AgentInstance
	syncStatePath string // path to sync_state.json
}

// newSeahorseContextManager creates a seahorse-backed ContextManager.
func newSeahorseContextManager(_ json.RawMessage, al *AgentLoop) (ContextManager, error) {
	if al == nil {
		return nil, fmt.Errorf("seahorse: AgentLoop is required")
	}

	// Resolve workspace for DB path
	// DB stores session data, so it goes in sessions/ directory
	agent := al.registry.GetDefaultAgent()
	dbPath := agent.Workspace + "/sessions/seahorse.db"

	// Create CompleteFn from provider
	completeFn := providerToCompleteFn(agent.Provider, agent.Model)

	// Create engine
	engine, err := seahorse.NewEngine(seahorse.Config{
		DBPath:           dbPath,
		FreshTailCount:   agent.SummarizeMessageThreshold,
		ContextThreshold: float64(agent.SummarizeTokenPercent) / 100.0,
	}, completeFn)
	if err != nil {
		return nil, fmt.Errorf("seahorse: create engine: %w", err)
	}

	mgr := &seahorseContextManager{
		engine:        engine,
		sessions:      agent.Sessions,
		agent:         agent,
		syncStatePath: filepath.Dir(dbPath) + "/sync_state.json",
	}

	// Register seahorse tools with the agent's tool registry
	retrieval := mgr.engine.GetRetrieval()
	al.RegisterTool(seahorse.NewGrepTool(retrieval))
	al.RegisterTool(seahorse.NewExpandTool(retrieval))

	// Bootstrap sessions incrementally using sync_state.json
	mgr.bootstrapSessions()

	return mgr, nil
}

// providerToCompleteFn wraps providers.LLMProvider as a seahorse.CompleteFn.
func providerToCompleteFn(provider providers.LLMProvider, model string) seahorse.CompleteFn {
	return func(ctx context.Context, prompt string, opts seahorse.CompleteOptions) (string, error) {
		resp, err := provider.Chat(
			ctx,
			[]providers.Message{{Role: "user", Content: prompt}},
			nil, // no tools for summarization
			model,
			map[string]any{
				"max_tokens":       opts.MaxTokens,
				"temperature":      opts.Temperature,
				"prompt_cache_key": "seahorse",
			},
		)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

// Assemble builds budget-aware context from seahorse SQLite.
func (m *seahorseContextManager) Assemble(ctx context.Context, req *AssembleRequest) (*AssembleResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("seahorse assemble: nil request")
	}

	budget := req.Budget
	if budget <= 0 {
		budget = 100000
	}

	// Reserve space for model response (spec lines 1400-1410)
	effectiveBudget := budget - req.MaxTokens
	if effectiveBudget <= 0 {
		// MaxTokens >= budget is a configuration problem
		// Use 50% as minimum to avoid guaranteed overflow
		logger.WarnCF("agent", "MaxTokens >= budget, using 50% fallback",
			map[string]any{"budget": budget, "max_tokens": req.MaxTokens})
		effectiveBudget = budget / 2
	}

	// MaxChatSizeWhenCompact may be zero when agent config did not override the
	// defaults (e.g. direct struct construction in tests). The seahorse assembler
	// already falls back to 8000 for non-positive values (short_assembler.go:67),
	// so passing 0 here is safe and keeps the contract identical.
	maxChatSizeWhenCompact := 0
	if m.agent != nil {
		maxChatSizeWhenCompact = m.agent.MaxChatSizeWhenCompact
	}

	result, err := m.engine.Assemble(ctx, req.SessionKey, seahorse.AssembleInput{
		Budget:                 effectiveBudget,
		MaxChatSizeWhenCompact: maxChatSizeWhenCompact,
	})
	if err != nil {
		return nil, fmt.Errorf("seahorse assemble: %w", err)
	}

	history := seahorseToProviderMessages(result)

	// Summary is already formatted as XML with system prompt addition by assembler
	return &AssembleResponse{
		History: history,
		Summary: result.Summary,
	}, nil
}

// Compact compresses conversation history via seahorse summarization.
func (m *seahorseContextManager) Compact(ctx context.Context, req *CompactRequest) error {
	if req == nil {
		return nil
	}

	// For retry (LLM overflow), use aggressive CompactUntilUnder to guarantee
	// context shrinks below budget (spec lines ~1410).
	if req.Reason == ContextCompressReasonRetry && req.Budget > 0 {
		_, err := m.engine.CompactUntilUnder(ctx, req.SessionKey, req.Budget)
		return err
	}

	_, err := m.engine.Compact(ctx, req.SessionKey, seahorse.CompactInput{
		Force:  req.Reason == ContextCompressReasonRetry,
		Budget: &req.Budget,
	})
	return err
}

// Ingest records a message into seahorse SQLite and updates sync state
// so that bootstrap can skip this session on next restart.
func (m *seahorseContextManager) Ingest(ctx context.Context, req *IngestRequest) error {
	if req == nil {
		return nil
	}

	msg := providerToSeahorseMessage(req.Message)
	_, err := m.engine.Ingest(ctx, req.SessionKey, []seahorse.Message{msg})
	if err == nil {
		m.syncStateUpdateIngest(req.SessionKey)
	}
	return err
}

// syncStateUpdateIngest updates sync_state.json after a new message has been
// ingested into a session. This keeps the file_size + message_count tracking
// current so bootstrapSessions can skip unchanged sessions on restart.
func (m *seahorseContextManager) syncStateUpdateIngest(sessionKey string) {
	jsonlPath := m.sessionsDir() + "/" + sanitizeSessionKey(sessionKey) + ".jsonl"
	info, err := os.Stat(jsonlPath)
	if err != nil {
		return
	}

	state := m.loadSyncState()
	entry := state[sessionKey]
	entry.FileSize = info.Size()
	entry.MessageCount++
	entry.UpdatedAt = time.Now().Format(time.RFC3339)
	state[sessionKey] = entry
	m.saveSyncState(state)
}

// Clear removes all stored context for a session (seahorse DB + JSONL).
func (m *seahorseContextManager) Clear(ctx context.Context, sessionKey string) error {
	if err := m.engine.ClearSession(ctx, sessionKey); err != nil {
		return err
	}
	if m.sessions != nil {
		m.sessions.SetHistory(sessionKey, []providers.Message{})
		m.sessions.SetSummary(sessionKey, "")
		return m.sessions.Save(sessionKey)
	}
	return nil
}

// bootstrapSessions reconciles all session JSONL files into seahorse SQLite,
// using sync_state.json to skip sessions whose JSONL file hasn't changed.
// This reduces startup from O(total_messages) to O(changed_messages).
func (m *seahorseContextManager) bootstrapSessions() {
	if m.sessions == nil {
		return
	}

	ctx := context.Background()
	syncState := m.loadSyncState()
	activeKeys := make(map[string]bool, len(syncState)+16)

	startTime := time.Now()
	sessions := m.sessions.ListSessions()
	logger.InfoCF("seahorse", "bootstrap sessions started", map[string]any{
		"session_count": len(sessions),
		"sync_count":    len(syncState),
	})

	var bootstrapped, skipped, cleared int

	for _, sessionKey := range sessions {
		activeKeys[sessionKey] = true
		jsonlPath := m.sessionsDir() + "/" + sanitizeSessionKey(sessionKey) + ".jsonl"
		info, err := os.Stat(jsonlPath)
		if err != nil {
			// JSONL file missing — meta may linger after a corrupted clear.
			// Try a fallback bootstrap (which will clear the DB), and
			// remove from syncState so it gets re-evaluated if recreated.
			_, _ = m.bootstrapSession(ctx, sessionKey)
			delete(syncState, sessionKey)
			bootstrapped++
			continue
		}

		entry, exists := syncState[sessionKey]
		if exists && entry.FileSize == info.Size() && entry.MessageCount > 0 {
			// File size matches and we have a recorded message count.
			// This is a strong hint to skip, but we could still have a corrupted DB.
			// However, for startup speed, we trust the sync_state if size hasn't changed.
			skipped++
			continue
		}

		// File is new or has changed — reconcile into seahorse DB
		count, err := m.bootstrapSession(ctx, sessionKey)
		if err != nil {
			// Bootstrap failed (e.g. DB locked); skip the syncState update
			// so we retry this session next startup.
			continue
		}

		bootstrapped++
		syncState[sessionKey] = syncStateEntry{
			FileSize:     info.Size(),
			MessageCount: count,
			UpdatedAt:    time.Now().Format(time.RFC3339),
		}
	}

	// Remove entries for sessions that no longer exist (e.g. cleared while
	// PicoClaw was stopped). Without this, sync_state.json grows stale keys
	// indefinitely.
	for key := range syncState {
		if !activeKeys[key] {
			// Reconciliation: if JSONL is gone, clear Seahorse DB too.
			if err := m.engine.ClearSession(ctx, key); err != nil {
				logger.WarnCF("seahorse", "failed to clear deleted session from DB", map[string]any{
					"session": key,
					"error":   err.Error(),
				})
				// Don't delete from syncState if clear failed, so we retry next boot.
				continue
			}
			cleared++
			delete(syncState, key)
		}
	}

	elapsed := time.Since(startTime)
	logger.InfoCF("seahorse", "bootstrap sessions completed", map[string]any{
		"duration_ms":  elapsed.Milliseconds(),
		"total":        len(sessions),
		"bootstrapped": bootstrapped,
		"skipped":      skipped,
		"cleared":      cleared,
	})

	m.saveSyncState(syncState)
}

// sessionsDir returns the path to the sessions directory containing JSONL files.
func (m *seahorseContextManager) sessionsDir() string {
	return filepath.Dir(m.syncStatePath)
}

// loadSyncState reads sync_state.json from the sessions directory.
// Returns an empty map if the file doesn't exist or is malformed.
func (m *seahorseContextManager) loadSyncState() map[string]syncStateEntry {
	data, err := os.ReadFile(m.syncStatePath)
	if err != nil {
		return make(map[string]syncStateEntry)
	}
	var state map[string]syncStateEntry
	if err := json.Unmarshal(data, &state); err != nil {
		return make(map[string]syncStateEntry)
	}
	return state
}

// saveSyncState atomically writes sync_state.json to disk.
func (m *seahorseContextManager) saveSyncState(state map[string]syncStateEntry) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if err := fileutil.WriteFileAtomic(m.syncStatePath, data, 0o644); err != nil {
		logger.WarnCF("seahorse", "save sync state", map[string]any{"error": err.Error()})
	}
}

// sanitizeSessionKey converts a session key to a safe filename component.
// Must match the convention in pkg/memory/jsonl.go:sanitizeKey.
func sanitizeSessionKey(key string) string {
	s := strings.ReplaceAll(key, ":", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// bootstrapSession reconciles JSONL session history into seahorse SQLite.
// Returns an error if the engine bootstrap fails, nil otherwise (including
// when there is no history to reconcile).
func (m *seahorseContextManager) bootstrapSession(ctx context.Context, sessionKey string) (int, error) {
	if m.sessions == nil {
		return 0, nil
	}

	history := m.sessions.GetHistory(sessionKey)
	if len(history) == 0 {
		// Differentiate between intentional wipe (file missing/empty) vs corruption (file has data but parse failed)
		jsonlPath := m.sessionsDir() + "/" + sanitizeSessionKey(sessionKey) + ".jsonl"
		info, err := os.Stat(jsonlPath)
		fileExists := err == nil
		fileEmpty := fileExists && info.Size() == 0

		if fileExists && !fileEmpty {
			// File has content but yielded 0 messages — likely corrupted JSONL. Do not wipe SQLite.
			err := fmt.Errorf("JSONL for session %q exists but yielded 0 parseable messages (possible corruption)", sessionKey)
			logger.WarnCF("seahorse", "bootstrap skipped to prevent data loss", map[string]any{
				"session": sessionKey,
				"error":   err.Error(),
			})
			return 0, err
		}

		// If history is empty and file is either empty or missing, ensure seahorse DB is also cleared.
		// ClearSession is a no-op internally if the session doesn't exist in DB.
		if err := m.engine.ClearSession(ctx, sessionKey); err != nil {
			logger.WarnCF("seahorse", "clear empty session failed", map[string]any{
				"session": sessionKey,
				"error":   err.Error(),
			})
			return 0, err
		}
		
		// Note: we don't log a success here because it runs on every boot for missing files unless we check DB existence first,
		// but ClearSession being a no-op is efficient enough (1 SELECT).
		return 0, nil
	}

	// Convert provider messages to seahorse messages
	msgs := make([]seahorse.Message, len(history))
	for i, h := range history {
		msgs[i] = providerToSeahorseMessage(h)
	}

	bootstrapStart := time.Now()
	if err := m.engine.Bootstrap(ctx, sessionKey, msgs); err != nil {
		logger.WarnCF("seahorse", "bootstrap failed", map[string]any{
			"session": sessionKey,
			"error":   err.Error(),
		})
		return 0, err
	}

	elapsed := time.Since(bootstrapStart)
	if elapsed > time.Second {
		logger.InfoCF("seahorse", "bootstrap session completed", map[string]any{
			"session":       sessionKey,
			"message_count": len(history),
			"duration_ms":   elapsed.Milliseconds(),
		})
	}
	return len(msgs), nil
}

// providerToSeahorseMessage converts a providers.Message to a seahorse.Message.
func providerToSeahorseMessage(msg protocoltypes.Message) seahorse.Message {
	result := seahorse.Message{
		Role:             msg.Role,
		Content:          msg.Content,
		ModelName:        msg.ModelName,
		ReasoningContent: msg.ReasoningContent,
		TokenCount:       tokenizer.EstimateMessageTokens(msg),
	}

	// Convert ToolCalls → MessageParts
	for _, tc := range msg.ToolCalls {
		part := seahorse.MessagePart{
			Type:       "tool_use",
			Name:       tc.Function.Name,
			Arguments:  tc.Function.Arguments,
			ToolCallID: tc.ID,
		}
		result.Parts = append(result.Parts, part)
	}

	// Convert tool result
	if msg.ToolCallID != "" {
		part := seahorse.MessagePart{
			Type:       "tool_result",
			ToolCallID: msg.ToolCallID,
			Text:       msg.Content,
		}
		result.Parts = append(result.Parts, part)
	}

	// Convert media attachments
	for _, mediaURI := range msg.Media {
		part := seahorse.MessagePart{
			Type:     "media",
			MediaURI: mediaURI,
		}
		result.Parts = append(result.Parts, part)
	}

	return result
}

// seahorseToProviderMessages converts a seahorse.AssembleResult to []providers.Message.
func seahorseToProviderMessages(result *seahorse.AssembleResult) []protocoltypes.Message {
	messages := make([]protocoltypes.Message, 0, len(result.Messages))

	// Convert assembled messages (which already include summary XML messages)
	for _, msg := range result.Messages {
		pm := protocoltypes.Message{
			Role:             msg.Role,
			Content:          msg.Content,
			ModelName:        msg.ModelName,
			ReasoningContent: msg.ReasoningContent,
		}

		// Reconstruct ToolCalls from parts
		for _, part := range msg.Parts {
			if part.Type == "tool_use" {
				pm.ToolCalls = append(pm.ToolCalls, protocoltypes.ToolCall{
					ID:   part.ToolCallID,
					Type: "function", // Required by OpenAI-compatible APIs (GLM, etc.)
					Function: &protocoltypes.FunctionCall{
						Name:      part.Name,
						Arguments: part.Arguments,
					},
				})
			}
			if part.Type == "tool_result" {
				pm.ToolCallID = part.ToolCallID
				if pm.Content == "" && part.Text != "" {
					pm.Content = part.Text
				}
			}
			if part.Type == "media" && part.MediaURI != "" {
				pm.Media = append(pm.Media, part.MediaURI)
			}
		}

		messages = append(messages, pm)
	}

	return messages
}

func init() {
	if err := RegisterContextManager("seahorse", newSeahorseContextManager); err != nil {
		panic(fmt.Sprintf("register seahorse context manager: %v", err))
	}
}
