#!/usr/bin/env python3
"""Mutation test for the MCP lifecycle fix (2026-11-16).

A test suite that passes proves nothing on its own. This script mutates the
production code in specific ways and asserts that the suite goes RED. If a
mutation stays GREEN, the corresponding test is not actually protecting the
behaviour it claims to.

Run:
    python3 pkg/mcp/mutation_test.py

Exit code 0 = every mutation was caught (good).
Exit code 1 = at least one mutation survived (test gap).

NOTE: this file deliberately contains no backslash-escape sequences for
newlines or tabs. Newlines are produced with chr(10) / chr(9) at runtime and
injected into mutation payloads through the @@NL@@ / @@TAB@@ placeholders.
Embedding them as literals is how the payload got corrupted twice during
development (contract C8).
"""

import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MANAGER = os.path.join(REPO, "pkg", "mcp", "manager.go")
PG_UNIX = os.path.join(REPO, "pkg", "mcp", "process_group_unix.go")

NL = chr(10)
TAB = chr(9)

# Each mutation: (name, file, old_regex, new_text, test_regex)
# Group 1 of old_regex, when present, is preserved in the output.
MUTATIONS = [
    (
        "P0a-revert-to-CommandContext",
        MANAGER,
        r"cmd := exec\.Command\(expandHomeCommandPath",
        r"cmd := exec.CommandContext(ctx, expandHomeCommandPath",
        r"TestR1_ProcessSurvivesCallerContextCancel",
    ),
    (
        "P0b-remove-idle-policy",
        MANAGER,
        r"if idle := m\.serverIdleTimeout\(conn\); idle > 0",
        r"if idle := m.serverIdleTimeout(conn); false && idle > 0",
        r"TestR2_IdleTimeoutKillsServer",
    ),
    (
        "P0b-remove-lifetime-policy",
        MANAGER,
        r"if maxLife := m\.serverMaxLifetime\(conn\); maxLife > 0",
        r"if maxLife := m.serverMaxLifetime(conn); false && maxLife > 0",
        r"TestR3_MaxLifetimeKillsServer",
    ),
    (
        "P0b-policy-always-default",
        MANAGER,
        r"d, err := time\.ParseDuration\(trimmed\)",
        r"d, err := time.ParseDuration(def.String())",
        r"TestR4_ZeroPolicyDisablesKill",
    ),
    (
        "P0b-ignore-per-server-override",
        MANAGER,
        r"if v := strings\.TrimSpace\(conn\.Config\.IdleTimeout\); v != \"\"",
        r"if v := strings.TrimSpace(conn.Config.IdleTimeout); false && v != \"\"",
        r"TestR5_PerServerPolicyOverridesGlobal",
    ),
    (
        "P1-disable-process-group",
        PG_UNIX,
        r"cmd\.SysProcAttr\.Setpgid = true",
        r"// mutation: process group disabled",
        r"TestR6_ProcessGroupKillsWrapperChildren",
    ),
    (
        "P1-kill-single-process-only",
        PG_UNIX,
        r"pgid := -pid",
        r"pgid := pid // mutation: kill only the direct child, not the group",
        r"TestR6c_KillServerKillsOrphan",
    ),
    (
        "P1-close-without-group-kill",
        MANAGER,
        r"killProcessGroup\(conn\.SubprocessPID, mcpKillGrace\)",
        r"// mutation: group kill removed",
        r"TestR6b_CloseKillsWholeGroup",
    ),
    (
        "P0c-procExited-ignores-death",
        MANAGER,
        r"(case <-c\.procExited:\s*)return false",
        r"\1return true",
        r"TestR7_ProcessIsReaped",
    ),
    (
        "P0a-reconnect-inherits-caller-ctx",
        MANAGER,
        r"reconnectCtx, reconnectCancel := context\.WithTimeout\(\s*context\.Background\(\), m\.reconnectTimeout\)",
        r"reconnectCtx, reconnectCancel := context.WithTimeout(ctx, m.reconnectTimeout)",
        r"TestR1e_ReconnectUsesIndependentContext",
    ),
]


def run_tests(test_regex):
    cmd = [
        "go", "test", "./pkg/mcp/", "-count=1", "-run", test_regex, "-timeout", "180s",
    ]
    proc = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True)
    return proc.returncode, proc.stdout + proc.stderr


def apply_mutation(content, old, new):
    """Apply the mutation to EVERY match.

    old is a regex. new may reference group 1 as \\1, and may use the
    @@NL@@ / @@TAB@@ placeholders for literal newlines and tabs.

    Every match is replaced, not just the first: the reconnect context
    appears in two branches (timeout and session-lost), and a mutation that
    only hits one of them is not a faithful representation of the bug.
    """
    pattern = re.compile(old, re.MULTILINE)

    def repl(match):
        replacement = new.replace("@@NL@@", NL).replace("@@TAB@@", TAB)
        if match.groups():
            replacement = replacement.replace("\\1", match.group(1))
        return replacement

    mutated, count = pattern.subn(repl, content)
    if count == 0:
        return None
    return mutated


def main():
    original = {}
    for _, path, _, _, _ in MUTATIONS:
        if path not in original:
            with open(path, "r", encoding="utf-8") as fh:
                original[path] = fh.read()

    survivors = []
    killed = 0

    try:
        for name, path, old, new, regex in MUTATIONS:
            with open(path, "r", encoding="utf-8") as fh:
                content = fh.read()

            mutated = apply_mutation(content, old, new)
            if mutated is None:
                print("SKIP     %-38s (pattern not found - mutation stale)" % name)
                survivors.append(name + " [STALE]")
                continue

            with open(path, "w", encoding="utf-8") as fh:
                fh.write(mutated)

            rc, out = run_tests(regex)
            if rc == 0:
                print("SURVIVED %-38s -> suite stayed GREEN (TEST GAP!)" % name)
                survivors.append(name)
            else:
                print("killed   %-38s -> suite went RED as expected" % name)
                killed += 1
            with open(path, "w", encoding="utf-8") as fh:
                fh.write(original[path])
    finally:
        for path, content in original.items():
            with open(path, "w", encoding="utf-8") as fh:
                fh.write(content)

    print("")
    print("=" * 64)
    print("mutations killed: %d / %d" % (killed, len(MUTATIONS)))
    if survivors:
        print("SURVIVORS (test gaps): %s" % ", ".join(survivors))
        return 1
    print("all mutations caught")
    return 0


if __name__ == "__main__":
    sys.exit(main())
