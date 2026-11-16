//go:build unix

package mcp

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// configureProcessGroup puts the MCP subprocess into its own process group so
// that killing the group also kills every wrapper child it spawned.
//
// Why this matters (measured 2026-11-16 on the live gateway):
//
//	DIRECT server (node, C binary)  -> SIGKILL closes the pipe -> EOF
//	WRAPPER server (uv, uvx, npx)  -> SIGKILL kills only the wrapper;
//	                                    the real server keeps the pipe open
//	                                    AND becomes an orphan (ppid=1).
//
// Observed live: 6 zombies "[uv] <defunct>" (ppid=gateway) + 5 orphan
// children (ppid=1) alive for 3h38m. The Manager's idle/lifetime policy can
// only kill what it tracks in m.servers, so orphaned grandchildren would live
// forever without a process group.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Setsid and Setpgid conflict, so set the flag on the existing struct
	// rather than replacing it. pkg/isolation does not set SysProcAttr on
	// Linux (verified 2026-11-16), and pkg/tools/shell_process_unix.go owns
	// a different command entirely.
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup terminates the whole process group rooted at pid.
//
// It first asks politely (SIGTERM), then escalates to SIGKILL after grace.
// It is a no-op when the group is already gone.
func killProcessGroup(pid int, grace time.Duration) {
	if pid <= 0 {
		return
	}
	// Negative pid => signal the whole process group.
	pgid := -pid

	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		// ESRCH means the group is already gone - nothing to escalate.
		return
	}
	if grace <= 0 {
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		return
	}

	// Poll for the group to disappear, then force-kill if it lingers.
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pgid, 0); err != nil {
			return // group gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}

// processGroupOf returns the process group id of pid.
func processGroupOf(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d", pid)
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read stat of %d: %w", pid, err)
	}
	s := string(b)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, fmt.Errorf("malformed stat for %d", pid)
	}
	fields := strings.Fields(s[idx+1:])
	// After ")" the fields are: state(0) ppid(1) pgrp(2) ...
	if len(fields) < 3 {
		return 0, fmt.Errorf("unexpected stat layout for %d", pid)
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, fmt.Errorf("parse pgrp for %d: %w", pid, err)
	}
	return pgid, nil
}

// killProcess sends SIGKILL to a single process (not its group).
func killProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
