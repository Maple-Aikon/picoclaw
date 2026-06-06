//go:build windows

package pid

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procGetExitCodeProcess         = kernel32.NewProc("GetExitCodeProcess")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	processQueryLimitedInformation = uint32(0x1000)
	stillActive                    = uint32(259)
)

// isOurGateway reports whether a live gateway process is bound to
// the given PID. On Windows there is no /proc filesystem, so we
// fall back to the OpenProcess + GetExitCodeProcess check that
// isProcessRunning already performs. Identity verification is not
// available; this is an inherent platform limitation.
func isOurGateway(pid int, hint IdentityHint) PidCheckResult {
	if isProcessRunning(pid) {
		return AliveOurs
	}
	return Stale
}

// isProcessRunning checks whether a process with the given PID is alive
// on Windows using OpenProcess + GetExitCodeProcess.
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}

	handle, _, _ := procOpenProcess.Call(
		uintptr(processQueryLimitedInformation),
		0,
		uintptr(pid),
	)
	if handle == 0 {
		return false
	}
	defer procCloseHandle.Call(handle)

	var exitCode uint32
	ret, _, _ := procGetExitCodeProcess.Call(handle, uintptr(unsafe.Pointer(&exitCode)))
	if ret == 0 {
		return false
	}
	return exitCode == stillActive
}

// currentBinaryName returns the basename of the running binary.
// On Windows, " (deleted)" markers are not used, so the value is
// returned as-is. Falls back to "" if the value cannot be
// determined.
func currentBinaryName() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Base(exe)
}

// currentComm returns the process name as Windows exposes it. There
// is no /proc/self/comm equivalent, so we fall back to the binary
// basename. The identity check is not enabled on Windows anyway.
func currentComm() string {
	return currentBinaryName()
}
