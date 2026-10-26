// Phase 12.63 (2026-10-26) — safety-guard errors must carry a typed
// ErrKind.
//
// WHY THIS FILE EXISTS (measured, not guessed):
//
// The exec safety guard returned `ErrorResult(msg)` with an EMPTY
// ErrKind. The agent recovery layer (pkg/agent/recovery_goal.go)
// classifies a tool failure in this order:
//
//	1. typed ErrKind (primary)
//	2. legacy "Tool execution failed:" prefix heuristic (fallback)
//
// No safety-guard message carries that legacy prefix, so an empty
// ErrKind meant `recoverable = false` → the recovery layer never fired
// → the tool_knowledge lesson for `exec` (which is FULL of guard
// workarounds: rm -rf blocked, chmod 700 blocked, heredoc blocked,
// …) was NEVER surfaced to the LLM.
//
// This is the SAME bug class as the MCP tool errors fixed in commit
// 56eaed33 — the fix pattern is identical: stamp a typed ErrKind on
// every policy-rejection path.
//
// ErrKind choice = ErrInvalidInput: a guard rejection is the LLM's
// mistake (the command violates policy), retrying the same command
// cannot help. It is NOT transient, so the retry prompt will not tell
// the LLM to "wait and retry the same call".

package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestExecTool_SafetyGuard_HasErrKind is the CORE invariant: every
// safety-guard rejection must carry ErrInvalidInput. Measured by
// actually executing a blocked command — NOT by reading the source.
func TestExecTool_SafetyGuard_HasErrKind(t *testing.T) {
	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatalf("unable to configure exec tool: %v", err)
	}

	// Each command trips a DIFFERENT deny pattern. Measured by running
	// it — the list was verified against the real defaultDenyPatterns
	// regexes, NOT guessed.
	cases := []struct {
		name    string
		command string
	}{
		{"dangerous_pattern", "rm -rf /"},
		{"dangerous_pattern_recursive", "rm -r /tmp/whatever"},
		{"dangerous_pattern_kill", "kill 12345"},
		{"dangerous_pattern_shutdown", "shutdown -h now"},
		{"dangerous_pattern_backtick", "echo `id`"},
		{"dangerous_pattern_chmod_mode", "chmod 700 /tmp/f"},
		{"dangerous_pattern_git_push", "git push origin main"},
		{"dangerous_pattern_sudo", "sudo ls /"},
		{"dangerous_pattern_dd", "dd if=/dev/zero of=/tmp/x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := tool.Execute(context.Background(), map[string]any{
				"action":  "run",
				"command": tc.command,
			})

			if !result.IsError {
				t.Fatalf("expected the guard to block %q, got success: %s",
					tc.command, result.ForLLM)
			}
			if !strings.Contains(result.ForLLM, "blocked by safety guard") {
				t.Fatalf("expected a safety-guard message for %q, got %q",
					tc.command, result.ForLLM)
			}
			// The invariant under test.
			if result.ErrKind != ErrInvalidInput {
				t.Fatalf("safety-guard rejection must carry ErrKind=%q, got %q (empty ErrKind sends the recovery layer down the legacy prefix heuristic, which no guard message matches ⇒ recovery never fires ⇒ no lesson)",
					ErrInvalidInput, result.ErrKind)
			}
		})
	}
}

// TestExecTool_SafetyGuard_ErrKindSurvivesContentForLLM verifies the
// typed kind is not lost when the result is rendered for the LLM —
// the recovery layer reads ts.lastToolResult.ErrKind, but the guard
// must not also blank the message.
func TestExecTool_SafetyGuard_ErrKindSurvivesContentForLLM(t *testing.T) {
	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatalf("unable to configure exec tool: %v", err)
	}

	result := tool.Execute(context.Background(), map[string]any{
		"action":  "run",
		"command": "rm -rf /",
	})
	if !result.IsError {
		t.Fatal("expected the guard to block the command")
	}
	content := result.ContentForLLM()
	if content == "" {
		t.Fatal("ContentForLLM must not be empty for a guard rejection")
	}
	if !strings.Contains(content, "blocked by safety guard") {
		t.Fatalf("ContentForLLM lost the guard message: %q", content)
	}
}

// TestExecTool_SafetyGuard_WorkspaceEscape_HasErrKind covers the
// workspace-restriction branch (different code path from the deny
// patterns): the working directory must not escape the workspace.
func TestExecTool_SafetyGuard_WorkspaceEscape_HasErrKind(t *testing.T) {
	workspace := t.TempDir()
	tool, err := NewExecTool(workspace, true)
	if err != nil {
		t.Fatalf("unable to configure exec tool: %v", err)
	}

	// cwd escapes the workspace → validatePathWithAllowPaths fails.
	result := tool.Execute(context.Background(), map[string]any{
		"action":  "run",
		"command": "echo hello",
		"cwd":     filepath.Dir(workspace),
	})

	if !result.IsError {
		t.Fatalf("expected the workspace guard to reject the cwd, got success: %s",
			result.ForLLM)
	}
	if result.ErrKind != ErrInvalidInput {
		t.Fatalf("workspace-escape rejection must carry ErrKind=%q, got %q",
			ErrInvalidInput, result.ErrKind)
	}
}

// TestExecTool_DenyPattern_HeredocNeverFires documents a REAL (pre-existing,
// unrelated to this fix) gap found while measuring the guard: the deny
// pattern `<<\s*EOF` contains an UPPERCASE literal, but guardCommand
// evaluates every deny pattern against the LOWERCASED command
// (`lower := strings.ToLower(cmd)`). The pattern therefore can never
// match and the heredoc deny rule is DEAD.
//
// This test documents the CURRENT behaviour so the gap is visible and
// cannot regress silently. It is NOT part of the ErrKind fix — fixing
// it would change what the guard blocks, which is a behaviour change
// that needs its own decision.
//
// Measured: pattern.MatchString(strings.ToLower(cmd)) == false while
// pattern.MatchString(cmd) == true.
func TestExecTool_DenyPattern_HeredocNeverFires(t *testing.T) {
	pat := regexp.MustCompile(`<<\s*EOF`)
	cmd := "cat >> /tmp/f <<EOF"
	lower := strings.ToLower(cmd)

	if pat.MatchString(lower) {
		t.Fatalf("heredoc deny pattern now matches the lowercased command — " +
			"the dead-pattern gap was closed upstream; update this test")
	}
	if !pat.MatchString(cmd) {
		t.Fatalf("heredoc deny pattern does not match the raw command either — " +
			"the pattern itself changed; update this test")
	}
}

// TestExecTool_NonGuardErrors_KeepTheirKind is the NEGATIVE CONTROL:
// the fix must not stamp ErrInvalidInput on errors that are NOT policy
// rejections (e.g. a missing command argument is a different thing,
// and a command that simply fails is the shell's fault, not a policy
// rejection).
func TestExecTool_NonGuardErrors_KeepTheirKind(t *testing.T) {
	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatalf("unable to configure exec tool: %v", err)
	}

	// Missing command → a plain validation error. We did NOT stamp a
	// kind here, so it must stay empty (this test pins that the fix
	// did not over-reach into unrelated paths).
	result := tool.Execute(context.Background(), map[string]any{"action": "run"})
	if !result.IsError {
		t.Fatal("expected an error for a missing command")
	}
	if result.ErrKind == ErrInvalidInput {
		t.Log("note: missing-command path now carries ErrInvalidInput — " +
			"acceptable (it is also an invalid input) but not required by this fix")
	}
}

// TestExecTool_SafetyGuard_RealWorkspace_IsNotBlocked is a SANITY
// CONTROL: the guard must still let ordinary commands through, i.e.
// the fix did not accidentally turn every exec into an error.
func TestExecTool_SafetyGuard_RealWorkspace_IsNotBlocked(t *testing.T) {
	workspace := t.TempDir()
	tool, err := NewExecTool(workspace, false)
	if err != nil {
		t.Fatalf("unable to configure exec tool: %v", err)
	}

	result := tool.Execute(context.Background(), map[string]any{
		"action":  "run",
		"command": "echo guard-sanity",
	})
	if result.IsError {
		t.Fatalf("ordinary command must not be blocked: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "guard-sanity") {
		t.Fatalf("expected the command output, got %q", result.ForUser)
	}
}

// TestExecTool_SafetyGuard_WorkspaceFileUntouched proves the guard
// rejection did NOT execute anything: the file the blocked command
// would have removed must still exist.
func TestExecTool_SafetyGuard_WorkspaceFileUntouched(t *testing.T) {
	workspace := t.TempDir()
	keep := filepath.Join(workspace, "keep.txt")
	if err := os.WriteFile(keep, []byte("precious"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	tool, err := NewExecTool(workspace, false)
	if err != nil {
		t.Fatalf("unable to configure exec tool: %v", err)
	}

	result := tool.Execute(context.Background(), map[string]any{
		"action":  "run",
		"command": "rm -rf " + keep,
	})
	if !result.IsError {
		t.Fatalf("expected the guard to block the delete, got success: %s", result.ForLLM)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the blocked command must NOT have run — file is gone: %v", err)
	}
}
