package pid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// tmpDir returns a clean temporary directory for a test.
func tmpDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pidtest-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestGenerateToken verifies that generateToken produces a 32-character hex string.
func TestGenerateToken(t *testing.T) {
	token := generateToken()
	if len(token) != 32 {
		t.Errorf("expected token length 32, got %d (token: %q)", len(token), token)
	}
	// Verify all characters are valid hex.
	for _, c := range token {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("token contains non-hex character: %c", c)
		}
	}
}

// TestGenerateTokenUniqueness checks that two consecutive tokens differ.
func TestGenerateTokenUniqueness(t *testing.T) {
	a := generateToken()
	b := generateToken()
	if a == b {
		t.Error("two consecutive tokens should not be equal")
	}
}

// TestPidFilePath returns the expected path.
func TestPidFilePath(t *testing.T) {
	dir := tmpDir(t)
	got := pidFilePath(dir)
	want := filepath.Join(dir, pidFileName)
	if got != want {
		t.Errorf("pidFilePath(%q) = %q, want %q", dir, got, want)
	}
}

// TestWritePidFile creates a PID file and verifies its contents.
func TestWritePidFile(t *testing.T) {
	dir := tmpDir(t)
	data, err := WritePidFile(dir, "127.0.0.1", 18790)
	if err != nil {
		t.Fatalf("WritePidFile failed: %v", err)
	}

	if data.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", data.PID, os.Getpid())
	}
	if data.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want %q", data.Host, "127.0.0.1")
	}
	if data.Port != 18790 {
		t.Errorf("Port = %d, want %d", data.Port, 18790)
	}
	if len(data.Token) != 32 {
		t.Errorf("Token length = %d, want 32", len(data.Token))
	}

	// Verify the file exists and can be unmarshalled.
	raw, err := os.ReadFile(filepath.Join(dir, pidFileName))
	if err != nil {
		t.Fatalf("failed to read pid file: %v", err)
	}

	var fileData PidFileData
	if err = json.Unmarshal(raw, &fileData); err != nil {
		t.Fatalf("failed to unmarshal pid file: %v", err)
	}
	if fileData.PID != data.PID || fileData.Token != data.Token {
		t.Error("file data mismatch")
	}

	// Verify file permissions (owner-only read/write).
	info, err := os.Stat(filepath.Join(dir, pidFileName))
	if err != nil {
		t.Fatalf("failed to stat pid file: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("file permission = %o, want 0600", perm)
	}
}

// TestWritePidFileOverwrite writes twice and verifies the PID file is replaced.
func TestWritePidFileOverwrite(t *testing.T) {
	dir := tmpDir(t)

	data1, err := WritePidFile(dir, "0.0.0.0", 18790)
	if err != nil {
		t.Fatalf("first WritePidFile failed: %v", err)
	}

	// Second write should succeed because the PID matches our process.
	data2, err := WritePidFile(dir, "0.0.0.0", 18800)
	if err != nil {
		t.Fatalf("second WritePidFile failed: %v", err)
	}

	if data2.Token == data1.Token {
		t.Error("token should change on re-write")
	}
	if data2.Port != 18800 {
		t.Errorf("Port = %d, want 18800", data2.Port)
	}
}

// TestWritePidFileStalePID writes a PID file with a non-running PID, then
// verifies WritePidFile cleans it up and writes a new one.
func TestWritePidFileStalePID(t *testing.T) {
	dir := tmpDir(t)

	// Write a PID file with a PID that almost certainly doesn't exist.
	stale := PidFileData{PID: 99999999, Token: "deadbeef12345678deadbeef12345678"}
	raw, _ := json.MarshalIndent(stale, "", "  ")
	os.WriteFile(filepath.Join(dir, pidFileName), raw, 0o600)

	data, err := WritePidFile(dir, "127.0.0.1", 18790)
	if err != nil {
		t.Fatalf("WritePidFile with stale PID failed: %v", err)
	}
	if data.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", data.PID, os.Getpid())
	}
}

// TestReadPidFileWithCheck verifies reading a valid PID file for the current process.
func TestReadPidFileWithCheck(t *testing.T) {
	dir := tmpDir(t)

	// Some sandboxed environments (e.g. macOS test runner) may restrict
	// signal(0), causing isProcessRunning(getpid()) to return false.
	if !isProcessRunning(os.Getpid()) {
		t.Skip("skipping: isProcessRunning(getpid()) is false in this environment")
	}

	written, err := WritePidFile(dir, "127.0.0.1", 18790)
	if err != nil {
		t.Fatalf("WritePidFile failed: %v", err)
	}

	read := ReadPidFileWithCheck(dir)
	if read == nil {
		t.Fatal("ReadPidFileWithCheck returned nil for current process")
	}
	if read.PID != written.PID || read.Token != written.Token {
		t.Error("read data doesn't match written data")
	}
}

// TestReadPidFileWithCheckNonexistent returns nil for missing file.
func TestReadPidFileWithCheckNonexistent(t *testing.T) {
	dir := tmpDir(t)
	data := ReadPidFileWithCheck(dir)
	if data != nil {
		t.Error("expected nil for nonexistent PID file")
	}
}

// TestReadPidFileWithCheckStalePID auto-cleans a PID file whose process is dead.
func TestReadPidFileWithCheckStalePID(t *testing.T) {
	dir := tmpDir(t)

	stale := PidFileData{PID: 99999999, Token: "deadbeef12345678deadbeef12345678"}
	raw, _ := json.MarshalIndent(stale, "", "  ")
	os.WriteFile(filepath.Join(dir, pidFileName), raw, 0o600)

	data := ReadPidFileWithCheck(dir)
	if data != nil {
		t.Error("expected nil for stale PID")
	}

	// File should be cleaned up.
	if _, err := os.Stat(filepath.Join(dir, pidFileName)); !os.IsNotExist(err) {
		t.Error("stale PID file should be removed")
	}
}

// TestReadPidFileWithCheckInvalidFile auto-cleans malformed PID file.
func TestReadPidFileWithCheckInvalidFile(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, pidFileName)
	os.WriteFile(path, []byte("not json"), 0o600)

	data := ReadPidFileWithCheck(dir)
	if data != nil {
		t.Error("expected nil for malformed pid file")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("malformed PID file should be removed")
	}
}

// TestRemovePidFile removes the PID file for the current process.
func TestRemovePidFile(t *testing.T) {
	dir := tmpDir(t)

	if _, err := WritePidFile(dir, "127.0.0.1", 18790); err != nil {
		t.Fatalf("WritePidFile failed: %v", err)
	}

	RemovePidFile(dir)

	if _, err := os.Stat(filepath.Join(dir, pidFileName)); !os.IsNotExist(err) {
		t.Error("PID file should be removed")
	}
}

// TestRemovePidFileDifferentPID does not remove a PID file owned by another process.
func TestRemovePidFileDifferentPID(t *testing.T) {
	dir := tmpDir(t)

	other := PidFileData{PID: 99999999, Token: "deadbeef12345678deadbeef12345678"}
	raw, _ := json.MarshalIndent(other, "", "  ")
	os.WriteFile(filepath.Join(dir, pidFileName), raw, 0o600)

	RemovePidFile(dir)

	if _, err := os.Stat(filepath.Join(dir, pidFileName)); os.IsNotExist(err) {
		t.Error("PID file should NOT be removed (different PID)")
	}
}

// TestRemovePidFileNonexistent does not error on missing file.
func TestRemovePidFileNonexistent(t *testing.T) {
	dir := tmpDir(t)
	// Should not panic or error.
	RemovePidFile(dir)
}

func TestRemovePidFileIfPID(t *testing.T) {
	dir := tmpDir(t)

	other := PidFileData{PID: 99999999, Token: "deadbeef12345678deadbeef12345678"}
	raw, _ := json.MarshalIndent(other, "", "  ")
	path := filepath.Join(dir, pidFileName)
	os.WriteFile(path, raw, 0o600)

	removed := RemovePidFileIfPID(dir, 99999999)
	if !removed {
		t.Fatal("expected RemovePidFileIfPID to remove matching pid file")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("PID file should be removed for matching expected PID")
	}
}

func TestRemovePidFileIfPIDMismatch(t *testing.T) {
	dir := tmpDir(t)

	other := PidFileData{PID: 99999999, Token: "deadbeef12345678deadbeef12345678"}
	raw, _ := json.MarshalIndent(other, "", "  ")
	path := filepath.Join(dir, pidFileName)
	os.WriteFile(path, raw, 0o600)

	removed := RemovePidFileIfPID(dir, 88888888)
	if removed {
		t.Fatal("expected RemovePidFileIfPID to keep non-matching pid file")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("PID file should NOT be removed for mismatching expected PID")
	}
}

// TestWritePidFileContainerPID1 verifies that a leftover PID file with PID 1
// (typical container entrypoint) is treated as stale and overwritten.
func TestWritePidFileContainerPID1(t *testing.T) {
	dir := tmpDir(t)

	stale := PidFileData{PID: 1, Token: "deadbeef12345678deadbeef12345678"}
	raw, _ := json.MarshalIndent(stale, "", "  ")
	os.WriteFile(filepath.Join(dir, pidFileName), raw, 0o600)

	data, err := WritePidFile(dir, "127.0.0.1", 18790)
	if err != nil {
		t.Fatalf("WritePidFile should treat PID 1 as stale, got error: %v", err)
	}
	if data.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", data.PID, os.Getpid())
	}
}

// TestReadPidFileWithCheckContainerPID1 verifies that a leftover PID file
// with PID 1 is treated as stale and cleaned up.
func TestReadPidFileWithCheckContainerPID1(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("test not meaningful when running as PID 1")
	}
	dir := tmpDir(t)

	stale := PidFileData{PID: 1, Token: "deadbeef12345678deadbeef12345678"}
	raw, _ := json.MarshalIndent(stale, "", "  ")
	os.WriteFile(filepath.Join(dir, pidFileName), raw, 0o600)

	data := ReadPidFileWithCheck(dir)
	if data != nil {
		t.Error("expected nil for PID 1 leftover")
	}

	if _, err := os.Stat(filepath.Join(dir, pidFileName)); !os.IsNotExist(err) {
		t.Error("PID 1 leftover file should be removed")
	}
}

// TestReadPidFileUnlockedInvalidJSON returns error for malformed content.
func TestReadPidFileUnlockedInvalidJSON(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, pidFileName)
	os.WriteFile(path, []byte("not json"), 0o600)

	_, err := readPidFileUnlocked(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// TestReadPidFileUnlockedInvalidPID returns error for non-positive PID.
func TestReadPidFileUnlockedInvalidPID(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, pidFileName)
	os.WriteFile(path, []byte(`{"pid": -1, "token": "a"}`), 0o600)

	_, err := readPidFileUnlocked(path)
	if err == nil {
		t.Error("expected error for invalid PID")
	}
}

// TestIsOurGatewayZeroPID returns Stale for invalid PIDs.
func TestIsOurGatewayZeroPID(t *testing.T) {
	if got := isOurGateway(0, IdentityHint{}); got != Stale {
		t.Errorf("isOurGateway(0) = %v, want %v", got, Stale)
	}
	if got := isOurGateway(-1, IdentityHint{}); got != Stale {
		t.Errorf("isOurGateway(-1) = %v, want %v", got, Stale)
	}
}

// TestIsOurGatewayDeadPID returns Stale for a PID that does not exist.
func TestIsOurGatewayDeadPID(t *testing.T) {
	if got := isOurGateway(99999999, IdentityHint{}); got != Stale {
		t.Errorf("isOurGateway(99999999) = %v, want %v", got, Stale)
	}
}

// TestIsOurGatewayPIDReuseDifferentProcess simulates the crash-reboot
// scenario: a leftover PID file points to a live process that is NOT
// picoclaw. Even though the PID is alive, the identity check must
// return Stale so a fresh gateway can take over.
//
// We probe PID 1 (init/systemd), which is virtually always alive on a
// running Linux box and whose comm / exe basename never matches the
// test runner. Skip the test if we are running as PID 1 itself.
func TestIsOurGatewayPIDReuseDifferentProcess(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("test not meaningful when running as PID 1")
	}
	// First check PID 1 is actually alive in this environment; skip
	// if not (e.g. a sandboxed runner).
	if !isProcessRunning(1) {
		t.Skip("PID 1 is not signalable in this environment")
	}

	hint := IdentityHint{
		BinaryName: "picoclaw",
		Comm:       "picoclaw",
	}
	if got := isOurGateway(1, hint); got != Stale {
		t.Errorf("isOurGateway(1, picoclaw hint) = %v, want %v", got, Stale)
	}
}

// TestIsOurGatewaySelfMatches confirms the identity check returns
// AliveOurs for the running test process when the hint matches the
// runner's own comm and binary name. This is the happy path: a real
// gateway seeing its own PID file.
func TestIsOurGatewaySelfMatches(t *testing.T) {
	// Sanity: identity hint with our own names should be accepted.
	hint := IdentityHint{
		BinaryName: currentBinaryName(),
		Comm:       currentComm(),
	}
	if hint.BinaryName == "" && hint.Comm == "" {
		t.Skip("cannot determine current binary identity in this environment")
	}
	// Skip when running as PID 1 (no useful comm value).
	if os.Getpid() == 1 {
		t.Skip("test not meaningful when running as PID 1")
	}
	if !isProcessRunning(os.Getpid()) {
		t.Skip("isProcessRunning(getpid()) is false in this environment")
	}
	if got := isOurGateway(os.Getpid(), hint); got != AliveOurs {
		t.Errorf("isOurGateway(self, matching hint) = %v, want %v", got, AliveOurs)
	}
}

// TestIsOurGatewayCommMismatch rejects a live process whose comm does
// not match the recorded hint, even when the exe basename would
// otherwise line up. We synthesize this by passing our own PID with
// a hint that has a wrong comm value.
func TestIsOurGatewayCommMismatch(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("test not meaningful when running as PID 1")
	}
	if !isProcessRunning(os.Getpid()) {
		t.Skip("isProcessRunning(getpid()) is false in this environment")
	}
	hint := IdentityHint{
		BinaryName: currentBinaryName(),
		Comm:       "definitely-not-our-process",
	}
	if got := isOurGateway(os.Getpid(), hint); got != Stale {
		t.Errorf("isOurGateway(self, wrong comm) = %v, want %v", got, Stale)
	}
}

// TestIsOurGatewayExeMismatch rejects a live process whose exe
// basename does not match the recorded hint.
func TestIsOurGatewayExeMismatch(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("test not meaningful when running as PID 1")
	}
	if !isProcessRunning(os.Getpid()) {
		t.Skip("isProcessRunning(getpid()) is false in this environment")
	}
	hint := IdentityHint{
		BinaryName: "definitely-not-our-binary",
		Comm:       currentComm(),
	}
	if got := isOurGateway(os.Getpid(), hint); got != Stale {
		t.Errorf("isOurGateway(self, wrong exe) = %v, want %v", got, Stale)
	}
}

// TestIsOurGatewayLegacyHint confirms the backward-compat path: a
// PID file with no recorded identity (legacy / pre-fix) accepts any
// live, non-zombie process. This avoids bricking older deployments
// after the upgrade.
func TestIsOurGatewayLegacyHint(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("test not meaningful when running as PID 1")
	}
	if !isProcessRunning(os.Getpid()) {
		t.Skip("isProcessRunning(getpid()) is false in this environment")
	}
	if got := isOurGateway(os.Getpid(), IdentityHint{}); got != AliveOurs {
		t.Errorf("isOurGateway(self, empty hint) = %v, want %v (backward compat)", got, AliveOurs)
	}
}

// TestWritePidFileContainsIdentity verifies that the JSON written to
// disk by WritePidFile carries the binary_name and comm fields. This
// is what the next gateway start will use for the identity check.
func TestWritePidFileContainsIdentity(t *testing.T) {
	dir := tmpDir(t)
	data, err := WritePidFile(dir, "127.0.0.1", 18790)
	if err != nil {
		t.Fatalf("WritePidFile failed: %v", err)
	}

	if data.BinaryName == "" {
		t.Error("BinaryName should be populated on this platform")
	}
	if data.Comm == "" {
		t.Error("Comm should be populated on this platform")
	}

	// Round-trip through the file to confirm JSON shape.
	raw, err := os.ReadFile(filepath.Join(dir, pidFileName))
	if err != nil {
		t.Fatalf("failed to read pid file: %v", err)
	}
	var decoded PidFileData
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if decoded.BinaryName != data.BinaryName {
		t.Errorf("BinaryName round-trip mismatch: got %q, want %q", decoded.BinaryName, data.BinaryName)
	}
	if decoded.Comm != data.Comm {
		t.Errorf("Comm round-trip mismatch: got %q, want %q", decoded.Comm, data.Comm)
	}
}

// TestIsZombieShape is a small unit test for the /proc stat parser.
// It does not require an actual zombie (creating one cleanly from
// a test is non-trivial); it only verifies the parser is happy
// with a well-formed stat line.
func TestIsZombieShape(t *testing.T) {
	// Synthetic stat line for a non-zombie process (state 'R').
	// Field 3 (state) appears after the closing ')' of comm.
	const liveStat = "1234 (cat) R 1 1234 1234 0 -1 4194304 100 0 0 0 0 0 0 0 20 0 1 0"
	if isZombieFromStat(liveStat) {
		t.Error("state R should not be classified as zombie")
	}
	const zombieStat = "1234 (cat) Z 1 1234 1234 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0"
	if !isZombieFromStat(zombieStat) {
		t.Error("state Z should be classified as zombie")
	}
	const malformed = "no parens here"
	if isZombieFromStat(malformed) {
		t.Error("malformed stat should not be classified as zombie")
	}
}
