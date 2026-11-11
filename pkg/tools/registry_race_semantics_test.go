package tools

// registry_race_semantics_test.go — B4: SEMANTIC race guard for the
// per-agent tool registry (2026-11-11).
//
// WHY A SEPARATE TEST, NOT JUST `-race`
//
// `go test -race` proves there is no DATA race (two goroutines touching
// the same memory without a lock). It does NOT prove CORRECTNESS.
//
// The hazard this file guards is a SEMANTIC race:
//
//	ToolRegistry.SetAllowlist() and SetPhase() write the registry's
//	allowlist + phase. SetPhaseAllowlist() writes BOTH under one lock
//	(plan checkpoint-misfire-fix §17.2, P1) precisely because the old
//	two-call sequence left a window in which the registry held
//	CHECKPOINT's allowlist with phase="" - a state matching NEITHER
//	phase.
//
// If two turns run concurrently on the SAME registry (a cron turn and
//	a chat turn on one agent instance), one turn's SetPhaseAllowlist
//	can land between the other's write and its own read, so a turn
//	belonging to OPEN can observe CHECKPOINT's allowlist and lose its
//	domain tools.
//
// `-race` CANNOT catch that: every access is properly locked, so
//	there is no data race to report. Only an ASSERTION about the
//	observed state can catch it.
//
// WHAT THIS FILE ASSERTS
//
//	1. CONCURRENCY SAFETY: concurrent SetPhaseAllowlist + IsAllowed +
//	   EffectiveToolCount + ToProviderDefs must not race. Run with
//	   `go test -race`; this file is the one that exercises the shared
//	   registry from several goroutines.
//
//	2. COHERENCE: a reader must never observe a COMBINATION of state
//	   that was never written. Concretely, the registry must never
//	   report "nothing at all is executable" - that combination is
//	   produced by NEITHER the OPEN state (domain tool allowed) NOR the
//	   CHECKPOINT state (lifecycle tool allowed), so seeing it means the
//	   reader observed a mix of two different writes.
//
// The `go test -race` job in .github/workflows/pr.yml runs on
// linux/amd64 because the race detector does not work on this
// machine's linux/arm64 (ThreadSanitizer: "unsupported VMA range").

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/phases"
)

const (
	// A domain tool that must be executable at OPEN.
	raceDomainTool = "read_file"
	// A lifecycle tool that is executable at CHECKPOINT.
	raceLifecycleTool = "goal_progress"
	// raceMinReads is the minimum number of concurrent observations a
	// race test must perform. A busy-wait loop alone is NOT enough:
	// the loop can finish before the reader goroutine is even
	// scheduled, which would make the test assert nothing. The
	// negative control (which proved the detector CAN fire) depends
	// on this number being large enough to land inside the window.
	raceMinReads = 20000
)

// waitForReads blocks until the reader goroutine has performed at
// least min observations, or until a generous deadline passes.
//
// Without this, the test can observe ZERO reads and pass vacuously.
func waitForReads(mu *sync.Mutex, reads *int, min int) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		mu.Lock()
		n := *reads
		mu.Unlock()
		if n >= min {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func raceRegistry() *ToolRegistry {
	r := NewToolRegistry()
	r.SetProjectionFrozen(true)
	r.Register(&mockRegistryTool{name: raceDomainTool, desc: "domain", params: map[string]any{"type": "object"}})
	r.Register(&mockRegistryTool{name: raceLifecycleTool, desc: "lifecycle", params: map[string]any{"type": "object"}})
	return r
}

// raceSnapshot is an ATOMIC view of the registry's phase-dependent
// state, taken under a SINGLE read lock.
//
// THIS IS THE POINT OF THE TEST. Reading the allowlist and the phase
// with two separate calls (e.g. IsAllowed() then GetPhase()) observes
// two DIFFERENT instants, so a "mismatch" between them proves
// nothing - the writer may simply have run in between.
//
// A torn read is only real if BOTH fields are observed together and
// the pair is one that was NEVER written. Because this test lives in
// the same package as ToolRegistry, it can take the read lock itself
// and copy both fields atomically.
type raceSnapshot struct {
	phase     string
	allowlist map[string]struct{}
	hasAllow  bool
}

// snapshot takes the atomic (phase, allowlist) view.
func snapshot(r *ToolRegistry) raceSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := raceSnapshot{phase: r.phase, hasAllow: r.allowlist != nil}
	if r.allowlist != nil {
		s.allowlist = make(map[string]struct{}, len(r.allowlist))
		for k := range r.allowlist {
			s.allowlist[k] = struct{}{}
		}
	}
	return s
}

// domainAllowed reports whether the domain tool would pass the gate
// for this snapshot.
func (s raceSnapshot) domainAllowed() bool {
	if s.phase == "post_final" {
		return false
	}
	if s.hasAllow {
		if _, ok := s.allowlist[raceDomainTool]; !ok {
			return false
		}
	}
	return true
}

// lifecycleAllowed reports whether the lifecycle tool would pass the
// gate for this snapshot.
func (s raceSnapshot) lifecycleAllowed() bool {
	if s.phase == "post_final" {
		return false
	}
	if s.hasAllow {
		if _, ok := s.allowlist[raceLifecycleTool]; !ok {
			return false
		}
	}
	return true
}

// TestRegistryRace_SemanticCoherenceUnderConcurrency drives the
// registry from several goroutines at once and asserts that every
// OBSERVED (phase, allowlist) pair is a state that was actually
// written.
//
// A torn read - the bug this file exists for - would show a pair that
// belongs to NEITHER write: e.g. phase="open" together with the
// CHECKPOINT allowlist, so the domain tool is blocked even though
// the phase says it should be allowed.
func TestRegistryRace_SemanticCoherenceUnderConcurrency(t *testing.T) {
	r := raceRegistry()

	// The two states we alternate between.
	type state struct {
		phase     string
		allowlist []string
	}
	states := []state{
		{phase: phases.PhaseOpen, allowlist: nil},
		{phase: phases.PhaseCheckpoint, allowlist: []string{raceLifecycleTool}},
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: flip between the two states.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s := states[i%len(states)]
			r.SetPhaseAllowlist(s.allowlist, s.phase)
		}
	}()

	var mu sync.Mutex
	reads := 0
	torn := 0
	var tornSamples []string

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// ONE atomic observation of both fields.
				s := snapshot(r)

				domainOK := s.domainAllowed()
				lifeOK := s.lifecycleAllowed()

				// Also exercise the public readers (these are the ones
				// the runtime actually calls).
				_ = r.IsAllowed(raceDomainTool)
				_ = r.EffectiveToolCount()
				_ = r.ToProviderDefs()

				mu.Lock()
				reads++
				// LEGAL PAIR 1 (OPEN):      phase=open,      no allowlist
				//                            -> domain allowed, lifecycle allowed.
				// LEGAL PAIR 2 (CHECKPOINT): phase=checkpoint, allowlist={goal_progress}
				//                            -> domain blocked, lifecycle allowed.
				//
				// The TORN state is the MIXTURE: phase=open with the
				// CHECKPOINT allowlist (domain blocked), or
				// phase=checkpoint with no allowlist (domain allowed).
				// Neither pair was ever written.
				tornHere := false
				if s.phase == phases.PhaseOpen && !domainOK {
					tornHere = true
				}
				if s.phase == phases.PhaseCheckpoint && domainOK {
					tornHere = true
				}
				// The lifecycle tool is allowed in BOTH legal states,
				// so it must never be blocked.
				if !lifeOK {
					tornHere = true
				}
				if tornHere {
					torn++
					if len(tornSamples) < 5 {
						tornSamples = append(tornSamples, fmt.Sprintf(
							"phase=%q hasAllow=%v domain=%v life=%v",
							s.phase, s.hasAllow, domainOK, lifeOK))
					}
				}
				mu.Unlock()
			}
		}()
	}

	// Wait until the reader has genuinely exercised the registry.
	waitForReads(&mu, &reads, raceMinReads)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if reads == 0 {
		t.Fatal("no reads happened - the test did not exercise anything")
	}
	t.Logf("concurrent reads: %d", reads)
	if torn > 0 {
		t.Errorf("TORN READ observed %d times: the registry exposed a (phase, allowlist) "+
			"pair that was never written. samples=%v", torn, tornSamples)
	}
}

// TestRegistryRace_MixedStateWindowIsUnreachable is the DIRECT guard
// for the reason SetPhaseAllowlist exists.
//
// The pre-P1 sequence was:
//
//	SetAllowlist(names)  -> allowlist = names, and phase CLEARED
//	SetPhase(phase)      -> phase = phase
//
// Two separate lock acquisitions, so between them the registry holds
// (allowlist = checkpoint's list, phase = "") - a pair that matches
// NEITHER phase. The P1 fix wrote both fields under ONE lock.
//
// This test observes the (phase, allowlist) pair ATOMICALLY and
// asserts the mixed state never appears.
//
// WHY IT IS NOT FLAKY: the writer performs millions of iterations and
// the reader spins as fast as it can, so the window is hit reliably
// when it exists. The negative control (swap SetPhaseAllowlist for
// the two-call sequence) proves the detector fires.
func TestRegistryRace_MixedStateWindowIsUnreachable(t *testing.T) {
	r := raceRegistry()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: flip between the two states, using the ONE-LOCK API.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				r.SetPhaseAllowlist(nil, phases.PhaseOpen)
			} else {
				r.SetPhaseAllowlist([]string{raceLifecycleTool}, phases.PhaseCheckpoint)
			}
		}
	}()

	var mu sync.Mutex
	reads := 0
	mixed := 0
	var samples []string

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s := snapshot(r)
			mu.Lock()
			reads++
			// MIXED STATE: an allowlist is present but the phase is
			// empty. This is exactly what SetAllowlist leaves behind
			// before SetPhase runs. With SetPhaseAllowlist the two
			// fields change together, so this pair is unreachable.
			if s.hasAllow && s.phase == "" {
				mixed++
				if len(samples) < 5 {
					samples = append(samples, fmt.Sprintf(
						"phase=%q hasAllow=%v", s.phase, s.hasAllow))
				}
			}
			mu.Unlock()
		}
	}()

	waitForReads(&mu, &reads, raceMinReads)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if reads == 0 {
		t.Fatal("no reads happened - the test did not exercise anything")
	}
	t.Logf("concurrent atomic snapshots: %d", reads)
	if mixed > 0 {
		t.Errorf("MIXED STATE observed %d times: the registry exposed "+
			"(allowlist set, phase empty) - the pre-P1 split-write window. samples=%v",
			mixed, samples)
	}
}

// TestRegistryRace_SetPhaseAllowlistFinalState asserts the RESULT of
// SetPhaseAllowlist: the phase and its allowlist take effect together.
//
// NOTE ON THE NAME: this checks the FINAL state, not the atomicity of
// the write. The atomicity is covered by
// TestRegistryRace_MixedStateWindowIsUnreachable above, which is the
// test that has a negative control proving it can actually fail.
func TestRegistryRace_SetPhaseAllowlistFinalState(t *testing.T) {
	r := raceRegistry()

	r.SetPhaseAllowlist([]string{raceLifecycleTool}, phases.PhaseCheckpoint)

	if r.IsAllowed(raceDomainTool) {
		t.Error("domain tool must be blocked at CHECKPOINT")
	}
	if !r.IsAllowed(raceLifecycleTool) {
		t.Error("lifecycle tool must be allowed at CHECKPOINT")
	}

	r.SetPhaseAllowlist(nil, phases.PhaseOpen)

	if !r.IsAllowed(raceDomainTool) {
		t.Error("domain tool must be allowed at OPEN")
	}
}
