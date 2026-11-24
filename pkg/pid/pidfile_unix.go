//go:build !windows

package pid

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// processLiveness probes the OS for the state of the given PID.
//
// Order matters here. signal(0) is a cheap existence check, so it is
// tried first; only when it succeeds do we pay for reading
// /proc/<pid>/stat to tell a live process apart from a zombie.
//
// A zombie still occupies its PID slot and still answers signal(0),
// so it must be detected explicitly — see isZombie.
func processLiveness(pid int) pidLiveness {
	if pid <= 0 {
		return pidDead
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return pidDead
	}
	// Signal(0) does not kill the process but checks existence on Unix.
	if err = p.Signal(syscall.Signal(0)); err != nil {
		var errno syscall.Errno
		// EPERM means the process exists but we are not allowed to
		// signal it — it is alive, just foreign.
		if errors.As(err, &errno) && errno == syscall.EPERM {
			return pidAlive
		}
		return pidDead
	}
	// The PID slot is occupied, but the process behind it may already
	// have exited without being reaped.
	if isZombie(pid) {
		return pidZombie
	}
	return pidAlive
}

// isZombie reports whether the given PID is a zombie (state 'Z' in
// /proc/<pid>/stat). The kernel has not reaped the entry yet but
// the process is dead and cannot own the PID file.
func isZombie(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		// Cannot read stat — assume not zombie (signal(0) just told
		// us the PID is alive).
		return false
	}
	return isZombieFromStat(string(raw))
}

// isZombieFromStat is defined in pidfile_stat.go (platform-neutral).

// isProcessRunning checks whether a process with the given PID is alive
// on Unix-like systems.
//
// A zombie counts as NOT running: it has already exited, it owns no
// port and it cannot be the gateway. Treating it as alive is what let
// a stale PID file block gateway startup.
func isProcessRunning(pid int) bool {
	return processLiveness(pid) == pidAlive
}

// isPicoclawProcess reads /proc/<pid>/comm to confirm the process name
// contains "picoclaw". Returns false when the comm file can be read and
// the name does not match (e.g., PID was reused by an unrelated process).
// Returns true if /proc/<pid>/comm is unreadable so the call site falls
// back to trusting the liveness check alone.
func isPicoclawProcess(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return true // cannot verify — trust liveness check
	}
	return strings.Contains(strings.TrimSpace(string(data)), "picoclaw")
}
