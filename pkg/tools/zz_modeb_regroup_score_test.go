package tools

// zz_modeb_regroup_score_test.go — Mode B 2026-11-23: per-entry REGROUP
// scorer for the live tool knowledge store.
//
// READ-ONLY. It never writes to the store.
//
// IT MIRRORS LoadForEscalation EXACTLY:
//	store.Read -> extractBody -> renderKnowledgeFile
//	             -> (if len(rendered) > cap) digestSections
//	             -> extractBody
// An entry UNDER the cap is NOT digested in production, so it is NOT
// digested here either. Digesting it anyway would report a fictional
// "starved section" for an entry that actually loads whole.
//
// WHY A GO SCORER AND NOT A PYTHON PORT:
// the digest allocates BYTES (never chars) and the entries are Vietnamese
// + emoji. A char-based port drifted by 22 B on 2026-11-23 (1967 vs the
// real 1945). Every number below comes from the PRODUCTION functions, so
// it cannot drift from what the runtime actually injects.
//
// PARITY (measured 2026-11-23): scoring the live `exec` entry with this
// file reproduces the 11-23 regroup report exactly —
// fixed=872, H2=8, payload=1953, slack=33.
//
// Usage:
//
//	TK_STORE_DIR=<workspace>/memory/tool_knowledge \
//	TK_R1_SET=<census>.json \
//	TK_SCORE_JSONL=/tmp/tk-metrics.jsonl \
//	go test ./pkg/tools/ -run TestZZModeBRegroupScore -count=1 -v
//
// TK_SCORE_PATH scores ONE file instead of the whole directory (used to
// score a CANDIDATE before it is written to the live store).
//
// TK_R1_SET is optional: without it `class` is "unknown" and the raw
// metrics are still produced (the Python front-end owns the
// R1/TOPIC/DEAD classification).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// tkEntryScore is the per-entry metric set.
type tkEntryScore struct {
	Name            string `json:"name"`
	Class           string `json:"class"`
	SrcBytes        int    `json:"src_bytes"`
	RenderedBytes   int    `json:"rendered_bytes"`
	OverCap         bool   `json:"over_cap"`
	H2              int    `json:"h2"`
	BodySections    int    `json:"body_sections"`
	Fixed           int    `json:"fixed"`
	Room            int    `json:"room"`
	BodyBudget      int    `json:"body_budget"`
	SharePerSec     int    `json:"share_per_section"`
	DigestBytes     int    `json:"digest_bytes"`
	PayloadBytes    int    `json:"payload_bytes"`
	Slack           int    `json:"slack"`
	ExcerptTotal    int    `json:"excerpt_total"`
	MinExcerpt      int    `json:"min_excerpt"`
	Starved         int    `json:"starved"`
	TitleLost       int    `json:"title_lost"`
	FirstLineCut    int    `json:"first_line_cut"`
	ClaimCut        int    `json:"claim_cut"`
	MinClaimMargin  int    `json:"min_claim_margin"`
	H3Total         int    `json:"h3_total"`
	H3BodyInPayload int    `json:"h3_body_in_payload"`
	MaxFirstLine    int    `json:"max_first_line"`
	MedianFirst     int    `json:"median_first_line"`
	Fallback        bool   `json:"fallback"`
	GenericTitles   int    `json:"generic_titles"`
}

// tkSectionScore is the per-section detail (only for requested entries).
type tkSectionScore struct {
	Heading    string `json:"heading"`
	TitleBytes int    `json:"title_bytes"`
	BodyBytes  int    `json:"body_bytes"`
	Excerpt    int    `json:"excerpt_bytes"`
	HasBody    bool   `json:"has_body"`
	FirstLine  int    `json:"first_line_bytes"`
	FirstCut   bool   `json:"first_line_cut"`
	Generic    bool   `json:"generic_title"`
}

// tkConcreteTokenRe matches a token that carries a CONCRETE identifier.
var tkConcreteTokenRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// tkIsGenericTitle reports a title that carries no concrete token.
// A title built only from prose words ("What goes wrong") is GENERIC:
// the lesson cannot be found by keyword, which is exactly the failure the
// 2026-11-23 regroup of `exec` fixed. A backtick-quoted token counts as
// concrete by construction.
func tkIsGenericTitle(heading string) bool {
	h := strings.TrimSpace(heading)
	if h == "" {
		return false
	}
	if strings.Contains(h, "`") {
		return false
	}
	for _, tok := range tkConcreteTokenRe.FindAllString(h, -1) {
		if strings.Contains(tok, "_") || strings.ContainsAny(tok, "0123456789") {
			return false
		}
		// camelCase: lowercase/digit followed by an uppercase letter.
		for i := 1; i < len(tok); i++ {
			if tok[i] >= 'A' && tok[i] <= 'Z' && tok[i-1] >= 'a' && tok[i-1] <= 'z' {
				return false
			}
		}
		// ALLCAPS of length >= 4.
		if len(tok) >= 4 && tok == strings.ToUpper(tok) {
			return false
		}
	}
	return true
}

// tkLoadR1Set reads the optional R1 census JSON.
// Expected shape: {"r1": ["exec", "cron", ...], ...}
func tkLoadR1Set(t *testing.T) map[string]bool {
	t.Helper()
	path := os.Getenv("TK_R1_SET")
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("TK_R1_SET %s: %v", path, err)
	}
	var doc struct {
		R1 []string `json:"r1"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("TK_R1_SET %s: parse: %v", path, err)
	}
	set := make(map[string]bool, len(doc.R1))
	for _, n := range doc.R1 {
		set[n] = true
	}
	return set
}

func TestZZModeBRegroupScore(t *testing.T) {
	nl := string(rune(10))
	r1set := tkLoadR1Set(t)

	detailSet := map[string]bool{}
	for _, n := range strings.Split(os.Getenv("TK_SCORE_DETAIL"), ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			detailSet[n] = true
		}
	}

	// Collect the entries to score: one file, or the whole directory.
	type target struct {
		name string
		path string
	}
	var targets []target
	if single := os.Getenv("TK_SCORE_PATH"); single != "" {
		targets = append(targets, target{
			name: strings.TrimSuffix(filepath.Base(single), ".md"),
			path: single,
		})
	} else {
		dir := os.Getenv("TK_STORE_DIR")
		if dir == "" {
			ws := liveStoreWorkspace(t)
			dir = filepath.Join(ws, DefaultKnowledgeDir)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Skipf("tool knowledge dir not present: %s", dir)
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir %s: %v", dir, err)
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			targets = append(targets, target{
				name: strings.TrimSuffix(e.Name(), ".md"),
				path: filepath.Join(dir, e.Name()),
			})
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	}

	scores := make([]tkEntryScore, 0, len(targets))
	details := map[string][]tkSectionScore{}
	for _, tg := range targets {
		raw, err := os.ReadFile(tg.path)
		if err != nil {
			t.Fatalf("read %s: %v", tg.path, err)
		}
		body := extractBody(string(raw))
		rendered := renderKnowledgeFile(tg.name, body, time.Now().UTC())

		sc := tkEntryScore{
			Name:           tg.name,
			SrcBytes:       len(raw),
			RenderedBytes:  len(rendered),
			OverCap:        len(rendered) > KnowledgeSizeCap,
			MinExcerpt:     -1,
			MinClaimMargin: -1,
		}
		switch {
		case r1set == nil:
			sc.Class = "unknown"
		case r1set[tg.name]:
			sc.Class = "r1"
		case strings.Contains(tg.name, "."):
			sc.Class = "topic"
		default:
			sc.Class = "dead"
		}

		// THE PRODUCTION PATH.
		// An entry under the cap is injected WHOLE: no digest, no plan.
		final := rendered
		if len(rendered) > KnowledgeSizeCap {
			head, srcBody := splitFrontmatter(rendered)
			secs := splitH2Sections(srcBody)
			room := KnowledgeSizeCap - len(head) - len(truncationMarker)
			plan := allocateDigest(secs, room)
			out, ok := renderDigestAlloc(head, plan, KnowledgeSizeCap)
			if ok {
				final = out
			} else {
				// Fallback: headings only, tail dropped, EVERY body lost.
				sc.Fallback = true
				final = headingsOnly(head, secs, KnowledgeSizeCap)
			}
			sc.H2 = len(secs)
			sc.Room = room
			sc.Fixed = tkFixedOverhead(secs)
			sc.BodyBudget = room - sc.Fixed
			sc.DigestBytes = len(final)
			sc.Slack = KnowledgeSizeCap - len(final)

			for i, s := range secs {
				if s.heading == "" {
					continue
				}
				if !strings.Contains(final, s.heading) {
					sc.TitleLost++
				}
				if tkIsGenericTitle(s.heading) {
					sc.GenericTitles++
				}
				bodyText := strings.TrimSpace(s.body)
				if bodyText == "" {
					continue
				}
				sc.BodySections++
				firstLine := strings.Split(bodyText, nl)[0]
				ex := 0
				has := false
				if plan[i].hasBody {
					ex = len(plan[i].excerpt)
					has = true
				} else {
					sc.Starved++
				}
				sc.ExcerptTotal += ex
				if has && (sc.MinExcerpt < 0 || ex < sc.MinExcerpt) {
					sc.MinExcerpt = ex
				}
				cut := has && ex < len(firstLine)
				if cut {
					sc.FirstLineCut++
				}
				// THE CLAIM is what must survive, not the whole line.
				// The excerpt always starts at body byte 0, so a long
				// first line is delivered only PARTIALLY. The bolded
				// span (or the first clause) is the part that teaches.
				claim := tkClaimBytes(bodyText)
				if has && ex < claim {
					sc.ClaimCut++
				}
				if has {
					if m := ex - claim; sc.MinClaimMargin < 0 || m < sc.MinClaimMargin {
						sc.MinClaimMargin = m
					}
				}
				if len(firstLine) > sc.MaxFirstLine {
					sc.MaxFirstLine = len(firstLine)
				}
				// H3 BODY SURVIVAL.
				//
				// The digest splits on H2 only. An H3 heading and its
				// body are just BODY TEXT of the enclosing H2 section,
				// and the excerpt only reaches the first line of that
				// body. So an H3 lesson in an R1 entry is usually
				// RUNTIME-DEAD: present in the file, absent from the
				// payload. Measured 2026-11-23: `exec` has 58 H3 and
				// 0 of their bodies reach the payload.
				//
				// A lesson that must auto-load belongs in the H2 TITLE
				// or the H2's FIRST BODY LINE - never in an H3 of an R1
				// entry.
				for _, h3 := range tkH3FirstLines(s.body) {
					sc.H3Total++
					if strings.Contains(final, h3) {
						sc.H3BodyInPayload++
					}
				}
				if detailSet[tg.name] {
					details[tg.name] = append(details[tg.name], tkSectionScore{
						Heading:    s.heading,
						TitleBytes: len(s.heading),
						BodyBytes:  len(bodyText),
						Excerpt:    ex,
						HasBody:    has,
						FirstLine:  len(firstLine),
						FirstCut:   cut,
						Generic:    tkIsGenericTitle(s.heading),
					})
				}
			}
		} else {
			// Under the cap: the WHOLE body is injected. Report the
			// headroom and the H2 shape, but no digest metrics.
			head, srcBody := splitFrontmatter(rendered)
			secs := splitH2Sections(srcBody)
			sc.H2 = len(secs)
			sc.Room = KnowledgeSizeCap - len(head) - len(truncationMarker)
			sc.Fixed = tkFixedOverhead(secs)
			sc.BodyBudget = sc.Room - sc.Fixed
			sc.DigestBytes = len(final)
			sc.Slack = KnowledgeSizeCap - len(final)
			for _, s := range secs {
				if s.heading == "" {
					continue
				}
				if tkIsGenericTitle(s.heading) {
					sc.GenericTitles++
				}
				bodyText := strings.TrimSpace(s.body)
				if bodyText == "" {
					continue
				}
				sc.BodySections++
				firstLine := strings.Split(bodyText, nl)[0]
				if len(firstLine) > sc.MaxFirstLine {
					sc.MaxFirstLine = len(firstLine)
				}
			}
		}
		sc.PayloadBytes = len(extractBody(final))
		sc.MedianFirst = tkMedianFirstLine(splitH2Sections(srcBodyOf(rendered)), nl)
		if sc.BodySections > 0 {
			sc.SharePerSec = sc.BodyBudget / sc.BodySections
		}
		if sc.MinExcerpt < 0 {
			sc.MinExcerpt = 0
		}
		scores = append(scores, sc)
	}

	if outPath := os.Getenv("TK_SCORE_JSONL"); outPath != "" {
		var b strings.Builder
		for _, sc := range scores {
			line, err := json.Marshal(sc)
			if err != nil {
				t.Fatalf("marshal %s: %v", sc.Name, err)
			}
			b.Write(line)
			b.WriteString(nl)
		}
		if err := os.WriteFile(outPath, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write %s: %v", outPath, err)
		}
		fmt.Println("wrote " + outPath)
	}
	if outPath := os.Getenv("TK_SCORE_DETAIL_JSON"); outPath != "" {
		raw, err := json.MarshalIndent(details, "", " ")
		if err != nil {
			t.Fatalf("marshal details: %v", err)
		}
		if err := os.WriteFile(outPath, raw, 0o644); err != nil {
			t.Fatalf("write %s: %v", outPath, err)
		}
		fmt.Println("wrote " + outPath)
	}

	fmt.Println("=== Mode B 2026-11-23 regroup score ===")
	fmt.Printf("entries=%d cap=%d"+nl, len(scores), KnowledgeSizeCap)
	fmt.Printf("%-52s %-8s %8s %5s %7s %8s %8s %6s %6s %6s %6s"+nl,
		"entry", "class", "srcB", "h2", "fixed", "bodyBud", "payload", "slack",
		"starv", "cut", "gen")
	for _, sc := range scores {
		fmt.Printf("%-52s %-8s %8d %5d %7d %8d %8d %6d %6d %6d %6d"+nl,
			sc.Name, sc.Class, sc.SrcBytes, sc.H2, sc.Fixed, sc.BodyBudget,
			sc.PayloadBytes, sc.Slack, sc.Starved, sc.FirstLineCut, sc.GenericTitles)
	}
}

// srcBodyOf returns the body of a rendered file (frontmatter stripped).
func srcBodyOf(rendered string) string {
	_, body := splitFrontmatter(rendered)
	return body
}

// tkNL is the newline byte. Package level so the helpers below do not
// need it threaded through as a parameter.
const tkNL = 10

// tkClaimBytes returns the byte length of the CLAIM of a section body:
// the first BOLD span (**...**) if the body opens with one, otherwise the
// first clause up to the first sentence delimiter.
//
// WHY: the digest excerpt always starts at body byte 0 and is cut at a
// word boundary. A long first line is therefore delivered only
// PARTIALLY. What must survive is the CLAIM - the part that teaches -
// not the whole line. Measured 2026-11-23 on the regrouped `exec`:
// 8/8 sections had their first line cut, but the bolded claim survived
// in every one, so "first line cut" alone is a FALSE alarm.
func tkClaimBytes(bodyText string) int {
	line := strings.SplitN(bodyText, string(rune(tkNL)), 2)[0]
	// Bold span first.
	if i := strings.Index(line, "**"); i >= 0 {
		rest := line[i+2:]
		if j := strings.Index(rest, "**"); j >= 0 {
			return i + 2 + j + 2
		}
	}
	// Otherwise: up to the first clause delimiter.
	for _, sep := range []string{". ", "; ", " ⇒", " =>", " —", " - "} {
		if i := strings.Index(line, sep); i > 0 {
			return i + len(sep)
		}
	}
	return len(line)
}

// tkFixedOverhead mirrors the fixed cost allocateDigest reserves:
// one line per heading (heading + newline) and one newline per
// body-bearing section.
func tkFixedOverhead(secs []h2Section) int {
	fixed := 0
	for _, s := range secs {
		if s.heading != "" {
			fixed += len(s.heading) + 1
		}
		if strings.TrimSpace(s.body) != "" {
			fixed++
		}
	}
	return fixed
}

// tkH3FirstLines returns the first body line of every H3 inside a
// section body.
//
// WHY: the digest splits on H2 only. An H3 heading and its body are
// just BODY TEXT of the enclosing H2 section, and the excerpt only
// reaches the first line of that body. So an H3 lesson in an R1 entry is
// usually RUNTIME-DEAD: present in the file, absent from the payload.
// Measured 2026-11-23: `exec` has 58 H3 and 0 of their bodies reach the
// payload.
func tkH3FirstLines(body string) []string {
	var out []string
	lines := strings.Split(body, string(rune(tkNL)))
	for i, l := range lines {
		s := strings.TrimSpace(l)
		if !strings.HasPrefix(s, "### ") {
			continue
		}
		// The first non-empty, non-heading line AFTER the H3.
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			out = append(out, t)
			break
		}
	}
	return out
}

// tkMedianFirstLine returns the median length of the first body line of
// each body-bearing section. It is the reference length an excerpt has to
// beat for a section's RULE to survive the digest.
func tkMedianFirstLine(secs []h2Section, nl string) int {
	var lens []int
	for _, s := range secs {
		bodyText := strings.TrimSpace(s.body)
		if bodyText == "" {
			continue
		}
		lens = append(lens, len(strings.Split(bodyText, nl)[0]))
	}
	if len(lens) == 0 {
		return 0
	}
	sort.Ints(lens)
	return lens[len(lens)/2]
}
