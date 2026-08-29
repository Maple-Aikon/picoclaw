package seahorse

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// withTempGraphitiDB sets GRAPHITI_QUEUE_DB to a temp file path and resets
// the singleton. The returned cleanup func restores env and closes the DB.
//
// All graphiti_test.go cases should use this helper to get a clean per-test
// queue without touching the production episodes.db.
func withTempGraphitiDB(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "episodes_test.db")

	prevEnvDB := os.Getenv(graphitiEnvDBPath)
	prevEnvEn := os.Getenv(graphitiEnvEnabled)
	if err := os.Setenv(graphitiEnvDBPath, dbPath); err != nil {
		t.Fatalf("setenv GRAPHITI_QUEUE_DB: %v", err)
	}
	if err := os.Setenv(graphitiEnvEnabled, "1"); err != nil {
		t.Fatalf("setenv GRAPHITI_ENABLED: %v", err)
	}

	// Reset singleton so the env vars take effect on next call.
	_ = CloseGraphitiQueue()
	resetGraphitiQueueForTest()

	cleanup := func() {
		_ = CloseGraphitiQueue()
		_ = os.Setenv(graphitiEnvDBPath, prevEnvDB)
		_ = os.Setenv(graphitiEnvEnabled, prevEnvEn)
		resetGraphitiQueueForTest()
	}
	return dbPath, cleanup
}

// openReadOnlyDB returns a *sql.DB handle for read-only verification of the
// temp queue. Uses the same driver so pragmas stay consistent.
func openReadOnlyDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn := path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open ro db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// --- Tests ---

func TestResolveGraphitiDBPath_Default(t *testing.T) {
	// Save and clear env to test default.
	prevDB := os.Getenv(graphitiEnvDBPath)
	prevEn := os.Getenv(graphitiEnvEnabled)
	t.Cleanup(func() {
		_ = os.Setenv(graphitiEnvDBPath, prevDB)
		_ = os.Setenv(graphitiEnvEnabled, prevEn)
	})
	_ = os.Unsetenv(graphitiEnvDBPath)
	_ = os.Unsetenv(graphitiEnvEnabled)

	got := resolveGraphitiDBPath()
	if got == "" {
		t.Fatal("expected non-empty default path")
	}
	// Default uses ~ which we expand to UserHomeDir.
	if got[0] != '/' {
		t.Errorf("expected absolute path, got %q", got)
	}
	if filepath.Base(got) != "episodes.db" {
		t.Errorf("expected episodes.db filename, got %q", filepath.Base(got))
	}
}

func TestResolveGraphitiDBPath_DisabledByEnv(t *testing.T) {
	prevEn := os.Getenv(graphitiEnvEnabled)
	t.Cleanup(func() {
		_ = os.Setenv(graphitiEnvEnabled, prevEn)
	})
	_ = os.Setenv(graphitiEnvEnabled, "0")

	if got := resolveGraphitiDBPath(); got != "" {
		t.Errorf("expected empty path when disabled, got %q", got)
	}
}

func TestResolveGraphitiDBPath_DisabledByFalse(t *testing.T) {
	prevEn := os.Getenv(graphitiEnvEnabled)
	t.Cleanup(func() {
		_ = os.Setenv(graphitiEnvEnabled, prevEn)
	})
	_ = os.Setenv(graphitiEnvEnabled, "false")

	if got := resolveGraphitiDBPath(); got != "" {
		t.Errorf("expected empty path when disabled=false, got %q", got)
	}
}

func TestResolveGraphitiDBPath_OverrideByEnv(t *testing.T) {
	prevDB := os.Getenv(graphitiEnvDBPath)
	prevEn := os.Getenv(graphitiEnvEnabled)
	t.Cleanup(func() {
		_ = os.Setenv(graphitiEnvDBPath, prevDB)
		_ = os.Setenv(graphitiEnvEnabled, prevEn)
	})
	_ = os.Unsetenv(graphitiEnvEnabled)
	_ = os.Setenv(graphitiEnvDBPath, "/tmp/custom_path.db")

	got := resolveGraphitiDBPath()
	if got != "/tmp/custom_path.db" {
		t.Errorf("expected override path, got %q", got)
	}
}

func TestEnqueueGraphitiEpisode_HappyPath(t *testing.T) {
	dbPath, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	rid, err := enqueueGraphitiEpisode("session-A", "summary text 1", "leaf")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if rid == "" {
		t.Fatal("expected non-empty episode id")
	}

	// Verify row exists with correct schema fields.
	db := openReadOnlyDB(t, dbPath)
	var (
		gotID     string
		gotStatus string
		gotJSON   string
	)
	err = db.QueryRow("SELECT id, status, payload_json FROM episodes WHERE id = ?", rid).Scan(
		&gotID, &gotStatus, &gotJSON,
	)
	if err != nil {
		t.Fatalf("query row: %v", err)
	}
	if gotID != rid {
		t.Errorf("id mismatch: got %q want %q", gotID, rid)
	}
	if gotStatus != "queued" {
		t.Errorf("status: got %q want %q", gotStatus, "queued")
	}
	if gotJSON == "" {
		t.Error("payload_json should not be empty")
	}

	// Verify payload contents — must contain content, name, source_description, group_id.
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotJSON), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["content"] != "summary text 1" {
		t.Errorf("payload.content: got %v", payload["content"])
	}
	if payload["group_id"] != graphitiGroupID {
		t.Errorf("payload.group_id: got %v want %v", payload["group_id"], graphitiGroupID)
	}
	if payload["source_description"] != graphitiSourcePrefix+":session-A" {
		t.Errorf("payload.source_description: got %v", payload["source_description"])
	}
	name, ok := payload["name"].(string)
	if !ok || name == "" {
		t.Errorf("payload.name: got %v", payload["name"])
	}
	// name should contain sessionKey, kind, and a timestamp-like string.
	for _, want := range []string{"Seahorse", "leaf", "session-A"} {
		if !contains(name, want) {
			t.Errorf("payload.name %q missing %q", name, want)
		}
	}
}

func TestEnqueueGraphitiEpisode_CondensedKind(t *testing.T) {
	dbPath, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	rid, err := enqueueGraphitiEpisode("session-B", "condensed summary", "condensed")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	db := openReadOnlyDB(t, dbPath)
	var payloadJSON string
	if err := db.QueryRow("SELECT payload_json FROM episodes WHERE id = ?", rid).Scan(&payloadJSON); err != nil {
		t.Fatalf("query: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	name, _ := payload["name"].(string)
	if !contains(name, "condensed") {
		t.Errorf("name should contain 'condensed', got %q", name)
	}
}

func TestRememberInGraphiti_EmptyContentSkipped(t *testing.T) {
	dbPath, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	// Should not enqueue or error, and should not initialize the DB handle.
	rememberInGraphiti("session-X", "", "leaf")

	// Verify the database file was never even created.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("expected db file NOT to exist for empty content, got stat err=%v", err)
	}
}

func TestRememberInGraphiti_HappyPath(t *testing.T) {
	dbPath, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	rememberInGraphiti("session-Y", "real summary", "leaf")

	// rememberInGraphiti is synchronous-enqueue + fire-forget at call level;
	// since we just need to verify the side-effect, poll briefly for the row.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		db := openReadOnlyDB(t, dbPath)
		var count int
		_ = db.QueryRow("SELECT COUNT(*) FROM episodes").Scan(&count)
		_ = db.Close()
		if count > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	// If we got here the row never appeared.
	db := openReadOnlyDB(t, dbPath)
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM episodes").Scan(&count)
	if count == 0 {
		t.Error("expected row to be enqueued by rememberInGraphiti")
	}
}

func TestOpenGraphitiQueue_DisabledReturnsNil(t *testing.T) {
	prevEn := os.Getenv(graphitiEnvEnabled)
	prevDB := os.Getenv(graphitiEnvDBPath)
	t.Cleanup(func() {
		_ = os.Setenv(graphitiEnvEnabled, prevEn)
		_ = os.Setenv(graphitiEnvDBPath, prevDB)
	})
	_ = os.Setenv(graphitiEnvEnabled, "0")
	_ = CloseGraphitiQueue()
	resetGraphitiQueueForTest()

	db, path := openGraphitiQueue()
	if db != nil {
		t.Errorf("expected nil DB when disabled, got handle")
	}
	if path != "" {
		t.Errorf("expected empty path when disabled, got %q", path)
	}
}

func TestEnqueueGraphitiEpisode_DisabledReturnsError(t *testing.T) {
	prevEn := os.Getenv(graphitiEnvEnabled)
	t.Cleanup(func() {
		_ = os.Setenv(graphitiEnvEnabled, prevEn)
	})
	_ = os.Setenv(graphitiEnvEnabled, "0")
	_ = CloseGraphitiQueue()
	resetGraphitiQueueForTest()

	_, err := enqueueGraphitiEpisode("session-Z", "content", "leaf")
	if err == nil {
		t.Error("expected error when queue disabled, got nil")
	}
}

func TestOpenGraphitiQueue_AutoBootstrapSchema(t *testing.T) {
	dbPath, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	// Sanity: file does not yet exist.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("expected dbPath not to exist before open, stat err=%v", err)
	}

	// First call should create the DB file and run DDL.
	if _, err := enqueueGraphitiEpisode("session-Q", "x", "leaf"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected db file after enqueue, stat err=%v", err)
	}

	// Verify schema is present.
	db := openReadOnlyDB(t, dbPath)
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='episodes'").Scan(&n); err != nil {
		t.Fatalf("sqlite_master query: %v", err)
	}
	if n != 1 {
		t.Errorf("expected episodes table to exist, got n=%d", n)
	}
}

func TestOpenGraphitiQueue_SingletonReuse(t *testing.T) {
	_, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	db1, _ := openGraphitiQueue()
	db2, _ := openGraphitiQueue()
	if db1 != db2 {
		t.Error("expected singleton: second call should return same handle")
	}
}

func TestEnqueueGraphitiEpisode_ConcurrentNoBusy(t *testing.T) {
	_, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	const (
		workers  = 16
		perWork  = 50
		expected = workers * perWork
	)

	var success, errCount int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < perWork; i++ {
				_, err := enqueueGraphitiEpisode(
					"concurrent-session",
					"summary",
					"leaf",
				)
				if err != nil {
					atomic.AddInt64(&errCount, 1)
				} else {
					atomic.AddInt64(&success, 1)
				}
			}
		}(w)
	}
	wg.Wait()

	if errCount > 0 {
		t.Errorf("expected 0 errors under concurrency, got %d", errCount)
	}
	if success != int64(expected) {
		t.Errorf("expected %d successes, got %d", expected, success)
	}
}

func TestEnqueueGraphitiEpisode_TimestampFormat(t *testing.T) {
	dbPath, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	before := time.Now().UTC().Add(-time.Second)
	rid, err := enqueueGraphitiEpisode("session-T", "x", "leaf")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	db := openReadOnlyDB(t, dbPath)
	var enqAt, createdAt, updatedAt string
	if err := db.QueryRow(
		"SELECT enqueued_at, created_at, updated_at FROM episodes WHERE id = ?", rid,
	).Scan(&enqAt, &createdAt, &updatedAt); err != nil {
		t.Fatalf("query: %v", err)
	}

	for _, ts := range []string{enqAt, createdAt, updatedAt} {
		parsed, perr := time.Parse(time.RFC3339Nano, ts)
		if perr != nil {
			t.Errorf("timestamp %q not RFC3339Nano: %v", ts, perr)
			continue
		}
		if parsed.Before(before) || parsed.After(after) {
			t.Errorf("timestamp %q outside [%v, %v]", ts, before, after)
		}
	}
	// All three timestamps must be equal for a fresh insert.
	if enqAt != createdAt || createdAt != updatedAt {
		t.Errorf("timestamps should match for fresh insert: enq=%q created=%q updated=%q",
			enqAt, createdAt, updatedAt)
	}
}

func TestEnqueueGraphitiEpisode_UniqueIDs(t *testing.T) {
	_, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	const n = 100
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		rid, err := enqueueGraphitiEpisode("s", "x", "leaf")
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if seen[rid] {
			t.Fatalf("duplicate rid %q at iter %d", rid, i)
		}
		seen[rid] = true
	}
}

func TestEnqueueGraphitiEpisode_PayloadIsValidJSON(t *testing.T) {
	dbPath, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	// Content with characters that could trip naive encoders:
	// quotes, escaped newline, and tab.
	content := "Hello \"quoted\" world\nNewline\tTab"
	rid, err := enqueueGraphitiEpisode("s-json", content, "leaf")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	db := openReadOnlyDB(t, dbPath)
	var payloadJSON string
	if err := db.QueryRow("SELECT payload_json FROM episodes WHERE id = ?", rid).Scan(&payloadJSON); err != nil {
		t.Fatalf("query: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v raw=%q", err, payloadJSON)
	}
	gotContent, _ := payload["content"].(string)
	if gotContent != content {
		t.Errorf("content roundtrip mismatch: got %q want %q", gotContent, content)
	}
}

func TestCloseGraphitiQueue_Idempotent(t *testing.T) {
	_, cleanup := withTempGraphitiDB(t)
	defer cleanup()

	// Open then close.
	if _, err := enqueueGraphitiEpisode("s", "x", "leaf"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := CloseGraphitiQueue(); err != nil {
		t.Errorf("first close: %v", err)
	}
	if err := CloseGraphitiQueue(); err != nil {
		t.Errorf("second close should be no-op, got: %v", err)
	}
}

func TestSetGraphitiConfig(t *testing.T) {
	dir := t.TempDir()
	customDB := filepath.Join(dir, "custom_episodes.db")
	customGroup := "custom_group_test"

	SetGraphitiConfig(customDB, customGroup)
	t.Cleanup(func() {
		resetGraphitiQueueForTest()
	})

	qPath, gID := GetGraphitiConfig()
	if qPath != customDB {
		t.Errorf("expected queuePath %q, got %q", customDB, qPath)
	}
	if gID != customGroup {
		t.Errorf("expected groupID %q, got %q", customGroup, gID)
	}

	// Verify resolveGraphitiDBPath and resolveGraphitiGroupID use configured values
	if got := resolveGraphitiDBPath(); got != customDB {
		t.Errorf("resolveGraphitiDBPath: got %q, want %q", got, customDB)
	}
	if got := resolveGraphitiGroupID(); got != customGroup {
		t.Errorf("resolveGraphitiGroupID: got %q, want %q", got, customGroup)
	}

	// Enqueue and check row
	rid, err := enqueueGraphitiEpisode("custom-session", "custom summary", "leaf")
	if err != nil {
		t.Fatalf("enqueue with custom config: %v", err)
	}
	db := openReadOnlyDB(t, customDB)
	var payloadJSON string
	if err := db.QueryRow("SELECT payload_json FROM episodes WHERE id = ?", rid).Scan(&payloadJSON); err != nil {
		t.Fatalf("query custom db: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["group_id"] != customGroup {
		t.Errorf("expected payload group_id %q, got %v", customGroup, payload["group_id"])
	}
}