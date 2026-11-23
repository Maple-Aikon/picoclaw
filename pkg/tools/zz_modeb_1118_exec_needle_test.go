package tools

// zz_modeb_1118_exec_needle_test.go — Mode B 2026-11-18 needle probe for
// exec.md.
//
// Question this run must answer with a MEASUREMENT, not an assumption:
//   does the new exec.md lesson (the 3 "window + denominator" measurement
//   traps found while re-running canary E) actually SURVIVE the 2 KB
//   auto-load digest?
//
// MEMORY.md's rule (2026-10-20 / 2026-11-16):
//   "Entry co san" != "se tu nap"  =>  every auto-load fix needs an E2E
//   test asserting the lesson ACTUALLY appears in the payload.
//   Also: "needle phai nam trong TIEU DE H2" and "TITLE BYTES = FIXED
//   OVERHEAD => KHONG tao H2 moi, bai moi vao section CU (H3)".
//
// The lesson was added as an H3 inside the EXISTING gateway.log H2 (H2 count
// stayed at 10). This probe measures whether the needles survive.
//
// READ ONLY.  Run:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace go test ./pkg/tools/ -run TestZZModeB1118ExecNeedle -count=1 -v
import (
	"fmt"
	"strings"
	"testing"
)

func TestZZModeB1118ExecNeedle(t *testing.T) {
	nl := string(rune(10))
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	entry := "exec"
	auto := store.LoadForEscalation(entry)
	fmt.Println("=== Mode B 2026-11-18 exec.md needle probe ===")
	fmt.Println("entry=" + entry + "  autoBytes=" + fmt.Sprint(len(auto)) +
		"  cap=" + fmt.Sprint(KnowledgeSizeCap))

	needles := []string{
		"bẫy đo",
		"cửa sổ",
		"canary_e2",
		"46,5",
		"6,7",
		"code echo",
		"mẫu số",
	}
	fmt.Println("--- needles in the AUTO-LOAD payload ---")
	missing := 0
	for _, n := range needles {
		ok := strings.Contains(auto, n)
		if !ok {
			missing++
		}
		fmt.Println("  " + n + " => " + fmt.Sprint(ok))
	}

	fmt.Println("--- body bytes per H2 section in the AUTO-LOAD payload ---")
	secs := splitH2Sections(auto)
	for i, s := range secs {
		h := s.heading
		if h == "" {
			h = "<preamble>"
		}
		if len(h) > 62 {
			h = h[:62]
		}
		fmt.Println("  [" + fmt.Sprint(i) + "] " + fmt.Sprint(len(s.body)) + " B  " + h)
	}

	full, _ := store.Read(entry)
	fmt.Println("--- store.Read(" + entry + ") bytes=" + fmt.Sprint(len(full)) +
		" (full body, no cap) ---")
	fmt.Println("  lesson present in FULL body: " +
		fmt.Sprint(strings.Contains(full, "canary_e2")))
	fmt.Println("  lesson present in AUTO-LOAD : " +
		fmt.Sprint(strings.Contains(auto, "canary_e2")))

	fmt.Println("--- does the owning H2 heading survive the digest? ---")
	owner := "Đo số trong `gateway.log`"
	fmt.Println("  owner H2 heading present in AUTO-LOAD: " +
		fmt.Sprint(strings.Contains(auto, owner)))
	idx := -1
	for i, s := range secs {
		if strings.Contains(s.heading, owner) {
			idx = i
			break
		}
	}
	if idx >= 0 {
		body := strings.ReplaceAll(secs[idx].body, nl, " ")
		if len(body) > 700 {
			body = body[:700]
		}
		fmt.Println("  owner section body kept in AUTO-LOAD: " +
			fmt.Sprint(len(secs[idx].body)) + " B")
		fmt.Println("  owner section body excerpt: " + body)
	}

	if missing > 0 {
		fmt.Println("RESULT: " + fmt.Sprint(missing) + "/" + fmt.Sprint(len(needles)) +
			" needles MISSING from the auto-load payload")
	} else {
		fmt.Println("RESULT: all " + fmt.Sprint(len(needles)) +
			" needles PRESENT in the auto-load payload")
	}
}
