package tools

// zz_live_store_helper_test.go — shared helper for the live-store probe
// tests (2026-11-11).
//
// WHY THIS FILE EXISTS
//
// Five `zz_*` test files in this package assert against the LIVE tool
// knowledge store at a hardcoded absolute path:
//
//	const ws = "/home/maple/.picoclaw/workspace"
//
// That path does not exist on a CI runner:
//
//   - `NewToolKnowledgeStore` calls `os.MkdirAll(root, 0o755)`, and
//     `/home` is owned by root with mode 0755 on the GitHub-hosted
//     ubuntu runner. A non-root runner CANNOT create `/home/maple`
//     => MkdirAll fails => the store constructor returns an error
//     => `t.Fatalf("store: %v", err)` => the whole test FAILS.
//   - Verified locally: `os.mkdir('/home/ci_probe_test_dir')` raises
//     `PermissionError: [Errno 13] Permission denied` as uid 1000.
//
// So these tests were RED on CI by construction, before any of them
// were even able to measure anything.
//
// THE FIX
//
// `liveStoreWorkspace(t)` resolves the workspace from the environment:
//
//	PICOCLAW_TEST_WORKSPACE  -> used as-is
//	PICOCLAW_HOME            -> $PICOCLAW_HOME/workspace
//	                         -> default ~/.picoclaw/workspace
//
// and SKIPS the test when none of those exist on disk. A skip is the
// honest outcome when the live store is not reachable: these tests
// measure a machine-specific artifact, so on a machine without it
// there is nothing to assert.
//
// This keeps the local signal (the store IS present on Maple's box)
// while making CI green for the right reason (no store => no claim)
// instead of green by accident (store => flaky volatile assert).
//
// The same opt-in pattern is already used by
// `d5_live_guard_probe_test.go` (PICOCLAW_LIVE_GUARD_PROBE=1).

import (
	"os"
	"path/filepath"
	"testing"
)

// liveStoreWorkspace returns the workspace directory holding the live
// tool knowledge store, or skips the test when it cannot be found.
//
// Precedence:
//
//	PICOCLAW_TEST_WORKSPACE  (explicit override, used by CI jobs that
//	                          materialise a fixture store)
//	PICOCLAW_HOME            (-> $PICOCLAW_HOME/workspace)
//	                          (-> ~/.picoclaw/workspace)
func liveStoreWorkspace(t *testing.T) string {
	t.Helper()

	candidates := make([]string, 0, 3)

	if ws := os.Getenv("PICOCLAW_TEST_WORKSPACE"); ws != "" {
		candidates = append(candidates, ws)
	}
	if home := os.Getenv("PICOCLAW_HOME"); home != "" {
		candidates = append(candidates, filepath.Join(home, "workspace"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".picoclaw", "workspace"))
	}

	for _, c := range candidates {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}

	t.Skipf("live tool knowledge store not reachable; tried %v", candidates)
	return ""
}

// liveStoreDir returns the directory that holds the live knowledge
// entries, or skips the test. It additionally requires that the
// directory actually exists, so a stray empty workspace cannot
// masquerade as a live store.
func liveStoreDir(t *testing.T) string {
	t.Helper()

	ws := liveStoreWorkspace(t)
	dir := filepath.Join(ws, DefaultKnowledgeDir)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Skipf("live knowledge dir not present: %s", dir)
	}
	return dir
}
