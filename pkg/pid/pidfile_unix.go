//go:build !windows

package pid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// isOurGateway decides whether the PID stored in a PID file still
// refers to a live gateway instance.
//
// Layers, evaluated in order. Any failure short-circuits to Stale:
//  1. PID ≤ 0 — invalid; never alive.
//  2. signal(0) — fast existence check.
//     - nil     → process exists, keep going.
//     - EPERM   → process exists but is not signalable; treat as
//                 foreign and stop (do not touch it).
//     - ESRCH   → no such process; stale.
//     - other   → assume stale.
//  3. Zombie state via /proc/<pid>/stat — a zombie is dead for our
//     purposes; the PID will be reaped by its parent.
//  4. Identity check via /proc/<pid>/comm and /proc/<pid>/exe
//     basename. The live process must match the hint recorded at
//     write time. This protects against PID reuse after a crash or
//     reboot, where the recorded PID may now belong to an unrelated
//     process (e.g. dbus, systemd).
func isOurGateway(pid int, hint IdentityHint) PidCheckResult {
	if pid <= 0 {
		return Stale
	}

	// Layer 2: signal(0) existence + signalability check.
	p, err := os.FindProcess(pid)
	if err != nil {
		// On Unix, FindProcess only fails for invalid PIDs.
		return Stale
	}
	sigErr := p.Signal(syscall.Signal(0))
	if sigErr != nil {
		var errno syscall.Errno
		if errors.As(sigErr, &errno) {
			switch errno {
			case syscall.ESRCH:
				// No such process.
				return Stale
			case syscall.EPERM:
				// Process exists but we may not signal it. Likely owned by
				// another user or a different namespace; do not treat as
				// our gateway.
				return Stale
			}
		}
		// Unknown error — play it safe and treat as stale.
		return Stale
	}

	// Layer 3: zombie check.
	if isZombie(pid) {
		return Stale
	}

	// Layer 4: identity check. If the hint is empty (legacy PID
	// file from before this fix), fall back to permissive match
	// based on process liveness — we already verified aliveness
	// above, so any live non-zombie process is accepted.
	if !hasRecordedIdentity(hint) {
		return AliveOurs
	}
	if !isMatchingProcess(pid, hint) {
		return Stale
	}

	return AliveOurs
}

// hasRecordedIdentity reports whether the hint carries enough
// information to perform a real identity check. An empty hint
// means the PID file was written by a binary that did not record
// identity, so the caller should fall back to permissive matching.
func hasRecordedIdentity(hint IdentityHint) bool {
	return hint.BinaryName != "" || hint.Comm != ""
}

// isZombie reports whether the given PID is a zombie (state 'Z' in
// /proc/<pid>/stat). The kernel has not reaped the entry yet but
// the process is dead and cannot own the PID file.
func isZombie(pid int) bool {
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	raw, err := os.ReadFile(statPath)
	if err != nil {
		// Cannot read stat — assume not zombie (signal(0) just told
		// us the PID is alive).
		return false
	}
	return isZombieFromStat(string(raw))
}

// isZombieFromStat inspects the contents of a /proc/<pid>/stat line
// and returns true if the state field (the byte right after the
// closing paren of the comm field) is 'Z'. Extracted so unit tests
// can drive the parser without spawning an actual zombie.
func isZombieFromStat(content string) bool {
	rparen := strings.LastIndex(content, ")")
	if rparen < 0 || rparen+1 >= len(content) {
		return false
	}
	state := content[rparen+2] // skip ') '
	return state == 'Z'
}

// isMatchingProcess returns true if the live process at pid
// matches the identity recorded in the hint. Both the comm and
// exe basename must match (when present in the hint) to count as
// the same gateway. Either signal alone is not enough — a process
// could be renamed in flight, or a different binary could
// coincidentally share a basename.
func isMatchingProcess(pid int, hint IdentityHint) bool {
	if hint.Comm != "" {
		commRaw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			return false
		}
		live := strings.TrimSpace(string(commRaw))
		if live != hint.Comm {
			return false
		}
	}
	if hint.BinaryName != "" {
		exeTarget, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			return false
		}
		// Trim a trailing " (deleted)" marker. The kernel appends
		// this when the on-disk file backing the binary has been
		// unlinked or replaced (e.g. after a rebuild + redeploy).
		cleaned := strings.TrimSuffix(exeTarget, " (deleted)")
		cleaned = strings.TrimSpace(cleaned)
		if filepath.Base(cleaned) != hint.BinaryName {
			return false
		}
	}
	return true
}

// isProcessRunning is kept for any external callers that only need a
// signal(0)-level check. Internal callers should prefer isOurGateway
// which also verifies identity and zombie state.
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.EPERM
}

// currentBinaryName returns the basename of the running binary, with
// any " (deleted)" marker trimmed. Falls back to "" if the value
// cannot be determined.
func currentBinaryName() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	cleaned := strings.TrimSuffix(exe, " (deleted)")
	return filepath.Base(strings.TrimSpace(cleaned))
}

// currentComm returns the trimmed contents of /proc/self/comm.
// Falls back to "" if /proc is not available.
func currentComm() string {
	raw, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
