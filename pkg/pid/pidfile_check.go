package pid

// PidCheckResult is the outcome of probing whether a PID is the
// gateway. It is used by the platform-specific isOurGateway helper.
type PidCheckResult int

const (
	// Stale means the PID is dead, a non-gateway process, a zombie,
	// or the running process is not a gateway. Safe to overwrite.
	Stale PidCheckResult = iota
	// AliveOurs means a live gateway process is running at that PID
	// and the caller must treat the singleton as already taken.
	AliveOurs
)

// IdentityHint describes what a recorded PID file's writer looked
// like at the time it claimed the PID. isOurGateway uses these
// hints to decide whether the live process at the recorded PID is
// a real gateway instance (and not, for example, a different
// process that the kernel reassigned the PID to after a crash or
// reboot).
//
// When both fields are empty, the PID file pre-dates identity
// tracking. isOurGateway falls back to a permissive match that
// accepts the live process as long as it is alive and not a
// zombie — this preserves backward compatibility for existing
// PID files written by older binaries.
type IdentityHint struct {
	// BinaryName is the basename of /proc/<writer>/exe at write
	// time, with any " (deleted)" suffix already trimmed.
	BinaryName string
	// Comm is the trimmed contents of /proc/<writer>/comm at write
	// time.
	Comm string
}

// identityHintFrom extracts the IdentityHint that was recorded in
// a PID file. Files written by older binaries do not have the
// binary_name/comm fields; in that case the returned hint is the
// zero value and isOurGateway falls back to permissive matching.
func identityHintFrom(data *PidFileData) IdentityHint {
	if data == nil {
		return IdentityHint{}
	}
	return IdentityHint{
		BinaryName: data.BinaryName,
		Comm:       data.Comm,
	}
}
