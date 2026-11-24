package pid

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const pidFileName = ".picoclaw.pid"

var errInvalidPidFile = errors.New("invalid pid file")

// PidFileData is the JSON structure stored in the PID file.
type PidFileData struct {
	PID     int    `json:"pid"`
	Token   string `json:"token"`
	Version string `json:"version"`
	Port    int    `json:"port"`
	Host    string `json:"host"`
}

var pidMu sync.Mutex

// pidLiveness describes the state of a recorded PID as seen by the OS.
//
// The distinction between pidAlive and pidZombie matters: a zombie
// still occupies its PID slot, still answers signal(0) and still keeps
// its /proc/<pid>/comm name, but it has already exited and can do no
// work. Treating a zombie as a live gateway is what allowed a stale
// PID file to block gateway startup.
//
// See docs/design/stale-pid-file-blocks-gateway-startup.md.
type pidLiveness int

const (
	// pidDead means no process occupies that PID any more.
	pidDead pidLiveness = iota
	// pidZombie means the process has exited but its parent has not
	// reaped it yet. The PID slot is still occupied and will only be
	// released when the parent reaps it.
	pidZombie
	// pidAlive means a live process occupies that PID.
	pidAlive
)

// processLiveness probes the OS for the state of the given PID.
// Implemented per platform in pidfile_unix.go / pidfile_windows.go.

// pidFilePath returns the absolute path for the PID file given the home directory.
func pidFilePath(homePath string) string {
	return filepath.Join(homePath, pidFileName)
}

// generateToken creates a cryptographically random 32-character hex token.
func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to something pseudo-random if crypto/rand fails
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// WritePidFile creates (or overwrites) the PID file atomically.
// It returns an error if another gateway instance appears to be running
// (a valid PID file exists with a live process).
func WritePidFile(homePath, host string, port int) (*PidFileData, error) {
	pidMu.Lock()
	defer pidMu.Unlock()

	pidPath := pidFilePath(homePath)

	// Check for existing PID file → singleton enforcement.
	if data, err := readPidFileUnlocked(pidPath); err == nil {
		if os.Getpid() != data.PID {
			logger.Infof("found pid file (PID: %d, version: %s)", data.PID, data.Version)
			// PID 1 is typically init/systemd on the host or the entrypoint
			// inside a container. When a container stops and leaves behind a
			// PID file on a shared volume, the host's PID 1 (init) would
			// pass the liveness check, blocking new gateway starts.
			// Treat recorded PID 1 as always stale.
			//
			// A zombie is also stale: it has already exited, but its PID
			// slot is still occupied because its parent has not reaped it
			// yet. Such a process still answers signal(0) and still keeps
			// its "picoclaw" comm name, so without this check it would be
			// mistaken for a live gateway and block startup.
			// See docs/design/stale-pid-file-blocks-gateway-startup.md.
			//
			// The liveness probe reads /proc/<pid>/stat, so it is done
			// once and reused for every branch below.
			liveness := pidDead
			if data.PID != 1 {
				liveness = processLiveness(data.PID)
			}
			switch {
			case data.PID == 1:
				logger.Warnf("found pid file (PID: 1) which is always treated as stale")
			case liveness == pidZombie:
				logger.Warnf("found pid file (PID: %d) but process is a zombie (already exited, not reaped)", data.PID)
			case liveness == pidAlive:
				// Verify the process is actually a picoclaw instance.
				// If the PID was reused by an unrelated process
				// (e.g. systemd-resolved after a kill -9), treat
				// the PID file as stale and proceed with startup.
				if isPicoclawProcess(data.PID) {
					return nil, fmt.Errorf("gateway is already running (PID: %d, version: %s)", data.PID, data.Version)
				}
				logger.Warnf("found pid file (PID: %d) but process is not picoclaw", data.PID)
			}
			logger.Warnf("not running (PID: %d) so will remove the pid file: %s", data.PID, pidPath)
		}
		// Stale PID file; process no longer exists → clean up.
		os.Remove(pidPath)
	}

	data := &PidFileData{
		PID:     os.Getpid(),
		Version: config.GetVersion(),
		Port:    port,
		Host:    host,
	}

	token := generateToken()
	data.Token = token

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pid file: %w", err)
	}

	// Ensure parent directory exists.
	dir := filepath.Dir(pidPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create pid directory: %w", err)
	}

	// Write atomically via temp file + rename.
	tmp := pidPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return nil, fmt.Errorf("failed to write pid file: %w", err)
	}
	if err := os.Rename(tmp, pidPath); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("failed to rename pid file: %w", err)
	}
	logger.Debugf("wrote pid file: %s success", pidPath)

	return data, nil
}

// ReadPidFileWithCheck reads the PID file and additionally checks if
// the recorded process is still alive. Returns nil if the file is
// missing, unreadable, or the process has exited.
func ReadPidFileWithCheck(homePath string) *PidFileData {
	pidMu.Lock()
	defer pidMu.Unlock()

	pidPath := pidFilePath(homePath)
	data, err := readPidFileUnlocked(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		if errors.Is(err, errInvalidPidFile) {
			logger.Warnf("invalid pid file, remove it: %s (%v)", pidPath, err)
			_ = os.Remove(pidPath)
			return nil
		}
		logger.Debugf("failed to read pid file: %s", err)
		return nil
	}

	// Treat PID 1 as stale when we are not PID 1 ourselves (container
	// leftover on a shared volume — host PID 1 is init, not gateway).
	if data.PID == 1 && os.Getpid() != 1 {
		logger.Debugf("stale container PID 1, remove pid file: %s", pidPath)
		os.Remove(pidPath)
		return nil
	}

	// A zombie has already exited but still occupies its PID slot, so it
	// owns no port and cannot serve the gateway. Treat it like a dead
	// process and clean the PID file up.
	// See docs/design/stale-pid-file-blocks-gateway-startup.md.
	switch processLiveness(data.PID) {
	case pidZombie:
		logger.Debugf("process is a zombie (already exited), remove pid file: %s", pidPath)
		os.Remove(pidPath)
		return nil
	case pidDead:
		logger.Debugf("process not running, remove pid file: %s", pidPath)
		os.Remove(pidPath)
		return nil
	case pidAlive:
	}

	return data
}

// RemovePidFile deletes the PID file (e.g. on graceful shutdown).
func RemovePidFile(homePath string) {
	pidMu.Lock()
	defer pidMu.Unlock()

	pidPath := pidFilePath(homePath)
	// Only remove if the PID matches our own process (avoid deleting
	// a file that belongs to a newer gateway instance).
	if data, err := readPidFileUnlocked(pidPath); err == nil {
		if data.PID != os.Getpid() {
			return
		}
	}

	logger.Infof("remove pid file: %s", pidPath)
	os.Remove(pidPath)
}

// RemovePidFileIfPID deletes the PID file only when the recorded PID matches
// expectedPID. It returns true when the file is removed successfully.
func RemovePidFileIfPID(homePath string, expectedPID int) bool {
	if expectedPID <= 0 {
		return false
	}

	pidMu.Lock()
	defer pidMu.Unlock()

	pidPath := pidFilePath(homePath)
	data, err := readPidFileUnlocked(pidPath)
	if err != nil {
		return false
	}
	if data.PID != expectedPID {
		return false
	}
	if err := os.Remove(pidPath); err != nil {
		return false
	}
	return true
}

// readPidFileUnlocked reads the PID file without acquiring the lock.
// Caller must hold pidMu.
func readPidFileUnlocked(pidPath string) (*PidFileData, error) {
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return nil, err
	}

	var data PidFileData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidPidFile, err)
	}

	// Validate PID is a positive integer.
	if data.PID <= 0 {
		return nil, fmt.Errorf("%w: pid=%d", errInvalidPidFile, data.PID)
	}

	return &data, nil
}
