// Tests for the exec safety guard's pattern case-folding (change B) and
// its quote-aware heredoc detection (change D).
//
// BACKGROUND (measured, not guessed)
//
// The guard used to compile deny/allow patterns case-SENSITIVELY but
// match them against the LOWERCASED command:
//
//	lower := strings.ToLower(cmd)
//	pattern.MatchString(lower)
//
// Every pattern containing an UPPERCASE literal was therefore a DEAD
// RULE — it could never fire. Measured on the built-in deny list: 1 of
// 41 patterns was dead (`<<\s*EOF`, dead since commit 01d694b9,
// 2026-02-18). The heredoc deny rule had never worked; it was not a
// regression.
//
// The fix compiles the PATTERN case-insensitively and matches the
// ORIGINAL command. The command is never lowercased — that would
// destroy information the guard needs and would make a case-sensitive
// pattern unexpressible.
//
// The heredoc rule moved from a pattern into CODE, because:
//
//  1. A pattern inside denyPatterns is bypassable: guardCommand evaluates
//     the custom allowlist FIRST and runs the deny loop only when the
//     command was not allow-listed. `git apply <<EOF` would pass while
//     `cat <<EOF` is blocked (test T11 pins this).
//  2. A pattern cannot be quote-aware. Making `<<\s*EOF` case-insensitive
//     makes it fire on `<<EOF` text inside a quoted string. Measured on
//     the real exec corpus (3098 commands): the pattern matches 3
//     commands and ALL 3 are inside a double-quoted region.
//
// The file below is the regression net for both changes.

package tools

import (
	"regexp"
	"regexp/syntax"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// newGuardTool builds an exec tool with the given custom deny/allow
// patterns. Deny patterns are ENABLED so that the built-in deny list is
// in play.
func newGuardTool(t *testing.T, customDeny, customAllow []string) *ExecTool {
	t.Helper()
	cfg := &config.Config{}
	cfg.Tools.Exec.EnableDenyPatterns = true
	cfg.Tools.Exec.AllowRemote = true
	cfg.Tools.Exec.CustomDenyPatterns = customDeny
	cfg.Tools.Exec.CustomAllowPatterns = customAllow

	tool, err := NewExecToolWithConfig("", false, cfg)
	if err != nil {
		t.Fatalf("NewExecToolWithConfig() error: %v", err)
	}
	return tool
}

// guardBlocks runs the command through the tool and reports whether the
// safety guard refused it.
//
// It calls guardCommand DIRECTLY rather than Execute: Execute also runs
// the command in a shell, and a shell failure (a command that does not
// exist, a here-string that dash does not understand) is not a safety
// rejection. Mixing the two would make the test assert on the shell.
func guardBlocks(t *testing.T, tool *ExecTool, command string) bool {
	t.Helper()
	return tool.guardCommand(command, "") != ""
}

// ---------------------------------------------------------------------------
// T1–T4: the heredoc rule itself (change D).
// ---------------------------------------------------------------------------

// TestGuard_Heredoc_UnquotedDelimiterBlocked is the POSITIVE control for
// the heredoc rule: an unquoted delimiter means the body is subject to
// shell expansion.
//
// It must FAIL on the pre-fix code, where the rule was the dead pattern
// `<<\s*EOF`.
func TestGuard_Heredoc_UnquotedDelimiterBlocked(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	cases := []struct {
		name    string
		command string
	}{
		{"plain", "cat <<EOF"},
		{"space_before_delimiter", "cat << EOF"},
		{"tab_strip_variant", "cat <<-EOF"},
		{"redirect_after", "cat <<EOF > /tmp/out.sh"},
		{"append_after", "cat >> /tmp/f <<EOF"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !guardBlocks(t, tool, tc.command) {
				t.Fatalf("expected %q to be blocked (unquoted heredoc delimiter)", tc.command)
			}
		})
	}
}

// TestGuard_Heredoc_QuotedDelimiterAllowed is the NEGATIVE control: a
// quoted delimiter suppresses expansion, so the command must pass.
//
// It must NOT fail on the pre-fix code — the dead pattern never fired, so
// these commands were always allowed.
func TestGuard_Heredoc_QuotedDelimiterAllowed(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	cases := []struct {
		name    string
		command string
	}{
		{"single_quoted", "cat <<'EOF'"},
		{"double_quoted", `cat <<"EOF"`},
		{"space_and_single_quoted", "cat << 'EOF'"},
		{"space_and_double_quoted", `cat << "EOF"`},
		{"tab_strip_quoted", "cat <<-'EOF'"},
		{"quoted_with_redirect", "cat <<'EOF' > /tmp/out.sh"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if guardBlocks(t, tool, tc.command) {
				t.Fatalf("expected %q to be allowed (quoted heredoc delimiter)", tc.command)
			}
		})
	}
}

// TestGuard_Heredoc_NonEOFDelimiterBlocked proves the rule is no longer
// delimiter-specific. The old pattern only knew the literal `EOF`.
func TestGuard_Heredoc_NonEOFDelimiterBlocked(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	for _, command := range []string{
		"cat <<PYEOF",
		"cat <<SCRIPT",
		"cat <<MARKER",
		"cat << 'PYEOF'",
	} {
		// The last one is quoted and MUST be allowed; it is listed
		// here only to document the difference from the first three.
		if command == "cat << 'PYEOF'" {
			continue
		}
		if !guardBlocks(t, tool, command) {
			t.Fatalf("expected %q to be blocked (unquoted non-EOF delimiter)", command)
		}
	}
}

// TestGuard_Heredoc_QuotedDelimiterInsidePipelineAllowed is a negative
// control for a quoted heredoc embedded in a longer pipeline.
func TestGuard_Heredoc_QuotedDelimiterInsidePipelineAllowed(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	command := "cat <<'EOF' | grep needle"
	if guardBlocks(t, tool, command) {
		t.Fatalf("expected %q to be allowed", command)
	}
}

// ---------------------------------------------------------------------------
// T5–T8: the false-positive guards.
// ---------------------------------------------------------------------------

// TestGuard_Heredoc_BitShiftNotBlocked guards the measured false positive
// of the rejected regex approach (`<<-?\s*[^'"[:space:]]`): a bit-shift
// like `1<<20` is not a heredoc.
func TestGuard_Heredoc_BitShiftNotBlocked(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	cases := []struct {
		name    string
		command string
	}{
		{"bare_bitshift", "echo 1<<20"},
		{"in_read_call", "chunk = f.read(1<<24)"},
		{"in_loop", "while h.read(1<<20): pass"},
		{"with_call", "python3 -c \"print(f.read(1<<22))\""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if guardBlocks(t, tool, tc.command) {
				t.Fatalf("expected %q to be allowed (bit-shift, not a heredoc)", tc.command)
			}
		})
	}
}

// TestGuard_Heredoc_OperatorInsideStringNotBlocked guards the other
// measured false positive: `<<` that sits inside a quoted argument is
// regex text, not a redirect operator.
func TestGuard_Heredoc_OperatorInsideStringNotBlocked(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	cases := []struct {
		name    string
		command string
	}{
		{"single_quoted_regex", `git log -S '<<\s*EOF'`},
		{"double_quoted_banner", `echo "=== tests mentioning heredoc/<<EOF ==="`},
		{"double_quoted_with_slash", `rg -n "ToLower|dangerous pattern detected|<<\\s*EOF"`},
		{"inside_python_code", "python3 -c \"print('<<EOF')\""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if guardBlocks(t, tool, tc.command) {
				t.Fatalf("expected %q to be allowed (`<<` inside a string)", tc.command)
			}
		})
	}
}

// TestGuard_Heredoc_HereStringNotBlocked pins the `<<<` exclusion. A
// here-string has no delimiter and no body — it is a distinct operator
// with a different risk profile.
func TestGuard_Heredoc_HereStringNotBlocked(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	for _, command := range []string{
		`cmd <<< "$var"`,
		`awk '{print FILENAME}' <<< "$CONTENT"`,
		"grep needle <<< \"$CONTENT\"",
	} {
		if guardBlocks(t, tool, command) {
			t.Fatalf("expected %q to be allowed (here-string, not a heredoc)", command)
		}
	}
}

// ---------------------------------------------------------------------------
// T9, T13, T14: the shell-interpreter sink (change D3).
// ---------------------------------------------------------------------------

// TestGuard_Heredoc_ShellSinkBlocked is the positive control for the
// sink rule: feeding a heredoc into a shell that executes its standard
// input is the strongest signal of intent — dangerous even when the
// delimiter is quoted, because quoting stops expansion, not execution.
func TestGuard_Heredoc_ShellSinkBlocked(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	cases := []struct {
		name    string
		command string
	}{
		{"bash_unquoted", "bash <<EOF"},
		{"bash_quoted", "bash <<'EOF'"},
		{"sh_unquoted", "sh <<EOF"},
		{"zsh", "zsh <<EOF"},
		{"absolute_path", "/bin/bash <<EOF"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !guardBlocks(t, tool, tc.command) {
				t.Fatalf("expected %q to be blocked (heredoc fed to a shell interpreter)", tc.command)
			}
		})
	}
}

// TestGuard_Heredoc_PipedToShellBlocked is the positive control that
// guards the blind spot of the earlier measurement: the sink is placed
// AFTER the heredoc operator, so a detector that only inspects the text
// BEFORE the `<<` cannot see it.
func TestGuard_Heredoc_PipedToShellBlocked(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	cases := []struct {
		name    string
		command string
	}{
		{"pipe_sh", "cat <<EOF | sh"},
		{"pipe_bash", "cat <<EOF | bash"},
		{"pipe_sh_quoted_delim", "cat <<'EOF' | sh"},
		{"pipe_sh_space", "cat <<EOF | sh -s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !guardBlocks(t, tool, tc.command) {
				t.Fatalf("expected %q to be blocked (heredoc piped to a shell interpreter)", tc.command)
			}
		})
	}
}

// TestGuard_Heredoc_QuotedInterpreterNotBlocked is the NEGATIVE control
// for the sink rule. A heredoc piped into a Python interpreter is a
// normal, safe idiom — 7 real occurrences in the measured corpus.
//
// The interpreter list deliberately contains only shells.
func TestGuard_Heredoc_QuotedInterpreterNotBlocked(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	cases := []struct {
		name    string
		command string
	}{
		{"python_quoted", "python3 - <<'PY'"},
		{"python_quoted_pipe", "cat <<'PY' | python3 -"},
		{"python_unquoted_is_still_dangerous", "python3 - <<PY"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked := guardBlocks(t, tool, tc.command)
			if tc.name == "python_unquoted_is_still_dangerous" {
				if !blocked {
					t.Fatalf("expected %q to be blocked (unquoted delimiter)", tc.command)
				}
				return
			}
			if blocked {
				t.Fatalf("expected %q to be allowed (python heredoc, not a shell)", tc.command)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// T11, T12: the placement of the heredoc rule (bypass-resistance).
// ---------------------------------------------------------------------------

// TestGuard_Heredoc_NotBypassableByAllowlist is THE test that proves the
// heredoc rule lives in code rather than in denyPatterns.
//
// guardCommand evaluates the custom allowlist FIRST and only runs the
// deny loop when the command was not allow-listed. A heredoc rule inside
// denyPatterns would therefore be skipped entirely for any command
// matching the allowlist: `git apply <<EOF` would pass while `cat <<EOF`
// is blocked.
//
// The allowlist below mirrors Maple's live `custom_allow_patterns`.
func TestGuard_Heredoc_NotBypassableByAllowlist(t *testing.T) {
	tool := newGuardTool(t, nil, []string{`^git\s+.*$`, `^gh\s+.*$`, `^bun\s+.*$`, `^uv\s+.*$`})

	if !guardBlocks(t, tool, "git apply <<EOF") {
		t.Fatalf("expected %q to be blocked: the heredoc rule must not be "+
			"bypassable by the custom allowlist", "git apply <<EOF")
	}
}

// TestGuard_Heredoc_AllowlistedQuotedStillAllowed is the negative control
// for the bypass test: an allow-listed command with a QUOTED heredoc stays
// allowed.
func TestGuard_Heredoc_AllowlistedQuotedStillAllowed(t *testing.T) {
	tool := newGuardTool(t, nil, []string{`^git\s+.*$`})

	if guardBlocks(t, tool, "git apply <<'EOF'") {
		t.Fatalf("expected %q to be allowed", "git apply <<'EOF'")
	}
}

// TestGuard_Heredoc_AllowlistedPlainCommandStillAllowed is a sanity
// control: the allowlist still works for ordinary commands.
func TestGuard_Heredoc_AllowlistedPlainCommandStillAllowed(t *testing.T) {
	tool := newGuardTool(t, nil, []string{`^git\s+.*$`})

	if guardBlocks(t, tool, "git status") {
		t.Fatalf("expected %q to be allowed", "git status")
	}
}

// ---------------------------------------------------------------------------
// T5, T10: change B — case-insensitive pattern matching.
// ---------------------------------------------------------------------------

// TestGuard_Pattern_CaseInsensitiveDeny proves change B at the
// compile-and-match level: a deny pattern with an UPPERCASE literal
// fires on both casings.
//
// The word is deliberately NOT "sudo": the built-in deny list already
// contains `\bsudo\b`, which would mask the effect of the custom
// pattern.
func TestGuard_Pattern_CaseInsensitiveDeny(t *testing.T) {
	tool := newGuardTool(t, []string{`\bFrobnicator\b`}, nil)

	for _, command := range []string{"frobnicator run", "FROBNICATOR run", "FrobnIcAtOr run"} {
		if !guardBlocks(t, tool, command) {
			t.Fatalf("expected %q to be blocked by the pattern %q", command, `\bFrobnicator\b`)
		}
	}
}

// TestGuard_Pattern_CaseInsensitiveAllow proves change B on the ALLOW
// side: an allow pattern with an UPPERCASE literal matches both casings.
func TestGuard_Pattern_CaseInsensitiveAllow(t *testing.T) {
	tool := newGuardTool(t, nil, []string{`^GIT\s+.*$`})

	for _, command := range []string{"git status", "GIT STATUS", "Git status"} {
		if guardBlocks(t, tool, command) {
			t.Fatalf("expected %q to be allowed by the pattern %q", command, `^GIT\s+.*$`)
		}
	}
}

// TestGuard_Pattern_CaseInsensitiveBuiltinDeny proves change B on the
// BUILT-IN deny list: `\brm\s+-[rf]{1,2}\b` must fire on an uppercase
// spelling too.
//
// Before the fix the built-in list was compiled case-sensitively and
// matched against a lowercased command, so this happened to work — but
// only by accident. The invariant is now stated directly.
func TestGuard_Pattern_CaseInsensitiveBuiltinDeny(t *testing.T) {
	tool := newGuardTool(t, nil, nil)

	for _, command := range []string{"rm -rf /tmp/x", "RM -RF /tmp/x", "Rm -r /tmp/x"} {
		if !guardBlocks(t, tool, command) {
			t.Fatalf("expected %q to be blocked by the built-in deny list", command)
		}
	}
}

// TestGuard_Pattern_CaseSensitiveOptOut proves the `(?-i)` escape hatch
// survives: a user can still demand case-sensitivity for one pattern.
//
// `(?-i)` turns case-folding OFF for the rest of the pattern, so
// `(?-i)\bFrobnicator\b` matches "Frobnicator" but NOT "frobnicator"
// and NOT "FROBNICATOR".
func TestGuard_Pattern_CaseSensitiveOptOut(t *testing.T) {
	tool := newGuardTool(t, []string{`(?-i)\bFrobnicator\b`}, nil)

	if !guardBlocks(t, tool, "Frobnicator run") {
		t.Fatalf("expected %q to be blocked", "Frobnicator run")
	}
	if guardBlocks(t, tool, "frobnicator run") {
		t.Fatalf("expected %q to be allowed: the pattern opted out of case-folding", "frobnicator run")
	}
	if guardBlocks(t, tool, "FROBNICATOR run") {
		t.Fatalf("expected %q to be allowed: the pattern opted out of case-folding", "FROBNICATOR run")
	}
}

// TestGuard_Pattern_CommandIsNotLowercased proves the SUBJECT is the
// original command, not a lowercased copy.
//
// The observable consequence: a case-sensitive pattern still sees the
// real casing. `(?-i)Frobnicator` matches "Frobnicator" but not
// "frobnicator" — which is only possible if the command was NOT
// lowercased before matching.
//
// (Under the old behaviour — lowercasing the command — the same pattern
// would have matched "frobnicator" and NOT "Frobnicator": the exact
// inverse.)
func TestGuard_Pattern_CommandIsNotLowercased(t *testing.T) {
	tool := newGuardTool(t, []string{`(?-i)\bFrobnicator\b`}, nil)

	if !guardBlocks(t, tool, "Frobnicator run") {
		t.Fatalf("expected %q to be blocked", "Frobnicator run")
	}
	if guardBlocks(t, tool, "frobnicator run") {
		t.Fatalf("expected %q to be allowed", "frobnicator run")
	}
}

// ---------------------------------------------------------------------------
// The dead-pattern detector, promoted from the measurement probe.
//
// It walks the parsed regexp AST and reports every UPPERCASE literal the
// pattern REQUIRES. Against a lowercased subject such a literal makes
// that branch unreachable: the pattern can never fire.
//
// FoldCase is a PER-NODE flag: with (?i) the flag lands on the literal
// node itself, not on the top-level concat, so it must be checked per
// node. (Checking the top-level node reported 37 of 41 patterns as dead
// — the detector was wrong twice before it was right.)
// ---------------------------------------------------------------------------

// deadPatternUpperLiterals returns the UPPERCASE literal bytes the
// pattern requires, or nil when it has none.
func deadPatternUpperLiterals(re *regexp.Regexp) []string {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return nil
	}
	var found []string
	var walk func(n *syntax.Regexp)
	walk = func(n *syntax.Regexp) {
		fold := n.Flags&syntax.FoldCase != 0
		switch n.Op {
		case syntax.OpLiteral:
			if fold {
				return
			}
			for _, r := range n.Rune {
				if r >= 'A' && r <= 'Z' {
					found = append(found, string(r))
				}
			}
		case syntax.OpCharClass:
			if fold {
				return
			}
			for _, r := range n.Rune {
				if r >= 'A' && r <= 'Z' {
					found = append(found, string(r))
				}
			}
		case syntax.OpConcat:
			for _, sub := range n.Sub {
				walk(sub)
			}
		case syntax.OpAlternate:
			for _, sub := range n.Sub {
				walk(sub)
			}
		case syntax.OpCapture:
			walk(n.Sub[0])
		case syntax.OpPlus, syntax.OpRepeat:
			walk(n.Sub[0])
		case syntax.OpQuest:
			walk(n.Sub[0])
		}
	}
	walk(parsed)
	return found
}

// TestGuard_DenyPatterns_NoDeadUppercaseLiterals is the regression test
// for the CLASS of bug, not for one pattern.
//
// No built-in deny pattern may require an uppercase literal: the guard
// matches patterns case-insensitively, so such a literal would still
// work — but a pattern list that keeps uppercase literals is a list that
// was written for the old, broken matching semantics, and the two can
// drift apart again the moment the matching semantics change.
func TestGuard_DenyPatterns_NoDeadUppercaseLiterals(t *testing.T) {
	lists := map[string][]*regexp.Regexp{
		"defaultDenyPatterns": defaultDenyPatterns,
		"windowsDenyPatterns": windowsDenyPatterns,
	}

	for name, list := range lists {
		for i, re := range list {
			if up := deadPatternUpperLiterals(re); len(up) > 0 {
				t.Errorf("%s[%d] %q requires uppercase literal(s) %v — a pattern "+
					"that requires an uppercase literal can never fire against a "+
					"lowercased subject", name, i, re.String(), up)
			}
		}
	}
}

// TestGuard_DeadPatternDetector_SelfCheck proves the detector is not
// vacuous. A detector that reports nothing is indistinguishable from a
// detector that is broken.
func TestGuard_DeadPatternDetector_SelfCheck(t *testing.T) {
	// A case-sensitive pattern with an uppercase literal IS dead.
	if got := deadPatternUpperLiterals(regexp.MustCompile(`<<\s*EOF`)); len(got) != 3 {
		t.Fatalf("detector self-check failed: expected 3 uppercase literals, got %v", got)
	}
	// A case-insensitive pattern is NOT dead.
	if got := deadPatternUpperLiterals(mustCompileCaseInsensitive(`<<\s*EOF`)); len(got) != 0 {
		t.Fatalf("detector self-check failed: a case-insensitive pattern must not be "+
			"reported as dead, got %v", got)
	}
	// A pattern with no uppercase literal is not dead either.
	if got := deadPatternUpperLiterals(regexp.MustCompile(`\brm\s+-[rf]{1,2}\b`)); len(got) != 0 {
		t.Fatalf("detector self-check failed: expected no uppercase literals, got %v", got)
	}
}
