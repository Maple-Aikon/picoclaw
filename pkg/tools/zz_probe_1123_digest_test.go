package tools

// zz_probe_1123_digest_test.go — READ-ONLY probe (2026-11-23).
//
// Purpose: measure the auto-load digest of an R1 entry BEFORE and AFTER an
// edit, so we can prove the edit did not push any existing needle out of the
// 2 KB auto-load payload.
//
// The digest keeps a PREFIX of each H2 section body (verified in
// pkg/tools/tool_knowledge.go allocateDigest/renderDigestAlloc), so a lesson
// appended at the END of a section body is invisible, and a large appended
// body can starve OTHER sections.
//
// Run:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace \
//   go test ./pkg/tools/ -run TestZZProbe1123Digest -count=1 -v
import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var probeNL = string(rune(10))

func TestZZProbe1123Digest(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	entry := "exec"
	auto := store.LoadForEscalation(entry)
	fmt.Println("=== 2026-11-23 digest probe (exec) ===")
	fmt.Println("autoBytes=" + fmt.Sprint(len(auto)) + " cap=" + fmt.Sprint(KnowledgeSizeCap))

	h2rx := regexp.MustCompile(`(?m)^## .+$`)
	heads := h2rx.FindAllString(auto, -1)
	parts := h2rx.Split(auto, -1)

	fmt.Println("--- H2 headings in AUTO-LOAD ---")
	for i, h := range heads {
		fmt.Println("  [" + fmt.Sprint(i) + "] " + h)
	}
	fmt.Println("H2 count in AUTO-LOAD: " + fmt.Sprint(len(heads)))

	fmt.Println("--- body bytes per H2 section in AUTO-LOAD ---")
	tot := 0
	for i := range heads {
		body := ""
		if i+1 < len(parts) {
			body = parts[i+1]
		}
		body = strings.TrimSpace(body)
		tot += len(body)
		fmt.Println("  [" + fmt.Sprint(i) + "] " + fmt.Sprint(len(body)) + " B  " + truncateProbe(body, 90))
	}
	fmt.Println("total body bytes in AUTO-LOAD: " + fmt.Sprint(tot))

	needles := []string{
		// pre-existing (2026-11-18 probe set)
		"bẫy đo", "cửa sổ", "canary_e2", "46,5", "6,7", "code echo", "mẫu số",
		// new (2026-11-23) candidate needles
		"đúng thiết kế", "escalate", "trigger upstream",
	}
	fmt.Println("--- needles in AUTO-LOAD ---")
	missing := 0
	for _, n := range needles {
		ok := strings.Contains(auto, n)
		if !ok {
			missing++
		}
		fmt.Println("  " + n + " => " + fmt.Sprint(ok))
	}
	fmt.Println("missing: " + fmt.Sprint(missing))

	full, err := store.Read(entry)
	if err != nil {
		fmt.Println("store.Read err: " + err.Error())
	} else {
		fmt.Println("store.Read(exec) bytes: " + fmt.Sprint(len(full)))
	}
}

func truncateProbe(s string, n int) string {
	s = strings.ReplaceAll(s, probeNL, " | ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
