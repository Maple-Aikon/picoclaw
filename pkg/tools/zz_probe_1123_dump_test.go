package tools

// zz_probe_1123_dump_test.go — READ-ONLY probe (2026-11-23).
// Dumps the exact auto-load payload of an R1 entry to /tmp so the byte
// layout can be inspected outside Go (needle offsets, prefix boundaries).
//
// Run:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace \
//   go test ./pkg/tools/ -run TestZZProbe1123Dump -count=1 -v
import (
	"os"
	"testing"
)

func TestZZProbe1123Dump(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	for _, entry := range []string{"exec", "tool_knowledge"} {
		payload := store.LoadForEscalation(entry)
		path := "/tmp/probe_1123_" + entry + ".payload.txt"
		if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("entry=%s bytes=%d -> %s", entry, len(payload), path)
	}
}
