/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/daeuniverse/dae/component/outbound/dialer"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
)

// fakeFailoverTimer is a fake failoverTimer produced by fakeFailoverScheduler.
// It records whether Stop was called and exposes the scheduled callback so
// the scheduler can fire it at the right logical time.
type fakeFailoverTimer struct {
	stopped bool
	fn      func()
}

func (t *fakeFailoverTimer) Stop() bool {
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// scheduledFailoverCall records one AfterFunc invocation's absolute deadline
// and the timer that holds its callback.
type scheduledFailoverCall struct {
	at    time.Time
	timer *fakeFailoverTimer
}

// fakeFailoverScheduler is a deterministic failoverScheduler used by the
// rotation state-machine tests. `now` is the logical clock; `pending` holds
// timers in the order they were created; `history` records every absolute
// logical deadline ever requested so tests can assert the full schedule
// even after a timer is stopped (stopped timers remain in history).
type fakeFailoverScheduler struct {
	now     time.Time
	pending []scheduledFailoverCall
	history []time.Time
}

func newFakeFailoverScheduler() *fakeFailoverScheduler {
	return &fakeFailoverScheduler{now: time.Unix(0, 0)}
}

func (s *fakeFailoverScheduler) Now() time.Time { return s.now }

func (s *fakeFailoverScheduler) AfterFunc(d time.Duration, fn func()) failoverTimer {
	timer := &fakeFailoverTimer{fn: fn}
	at := s.now.Add(d)
	s.pending = append(s.pending, scheduledFailoverCall{at: at, timer: timer})
	s.history = append(s.history, at)
	return timer
}

// Advance moves the logical clock forward by d. It does NOT fire timers.
func (s *fakeFailoverScheduler) Advance(d time.Duration) { s.now = s.now.Add(d) }

// FireNext fires the earliest pending timer, skipping any that were stopped.
// It advances the logical clock to the timer's scheduled deadline if the
// clock is behind. It fails the test if no pending timer exists.
func (s *fakeFailoverScheduler) FireNext(t *testing.T) {
	t.Helper()
	for len(s.pending) > 0 {
		call := s.pending[0]
		s.pending = s.pending[1:]
		if call.timer.stopped {
			continue
		}
		if call.at.After(s.now) {
			s.now = call.at
		}
		call.timer.stopped = true
		call.timer.fn()
		return
	}
	t.Fatal("no pending failover timer")
}

// PendingCount returns the number of currently pending (not-yet-fired) timers.
func (s *fakeFailoverScheduler) PendingCount() int {
	n := 0
	for _, c := range s.pending {
		if !c.timer.stopped {
			n++
		}
	}
	return n
}

// newRotationControllerTest builds a FailoverController with three named
// primary candidates (A, B, C) plus a fixed fallback, injects the fake
// scheduler, and returns the controller along with the candidate dialers so
// tests can script target-aware probe results.
func newRotationControllerTest(t *testing.T, cfg FailoverRecoveryConfig) (
	*FailoverController, *fakeFailoverScheduler, []*dialer.Dialer, *dialer.Dialer,
) {
	t.Helper()
	option := testFailoverDialerOption()
	candidates := []*dialer.Dialer{
		newNamedDirectDialer(option, "A"),
		newNamedDirectDialer(option, "B"),
		newNamedDirectDialer(option, "C"),
	}
	fallback := newNamedDirectDialer(option, "fallback")

	fc := NewFailoverControllerWithCandidates(log, "rotation-test", candidates, fallback, cfg)
	sched := newFakeFailoverScheduler()
	fc.scheduler = sched
	return fc, sched, candidates, fallback
}

// newRotationControllerWithLogger is like newRotationControllerTest but uses a
// caller-supplied logger so structured-log tests can attach a Logrus test hook.
func newRotationControllerWithLogger(t *testing.T, logger *logrus.Logger, cfg FailoverRecoveryConfig) (
	*FailoverController, *fakeFailoverScheduler, []*dialer.Dialer, *dialer.Dialer,
) {
	t.Helper()
	option := testFailoverDialerOption()
	candidates := []*dialer.Dialer{
		newNamedDirectDialer(option, "A"),
		newNamedDirectDialer(option, "B"),
		newNamedDirectDialer(option, "C"),
	}
	fallback := newNamedDirectDialer(option, "fallback")

	fc := NewFailoverControllerWithCandidates(logger, "rotation-test", candidates, fallback, cfg)
	sched := newFakeFailoverScheduler()
	fc.scheduler = sched
	return fc, sched, candidates, fallback
}

// TestFailoverRotationFifthFailureAdvancesToB verifies the exact exponential
// backoff schedule, that the fifth failure activates rotation and advances
// recoveryTarget from A (index 0) to B (index 1), that the active dialer
// remains the fixed fallback, and that at most one probe is ever in flight.
func TestFailoverRotationFifthFailureAdvancesToB(t *testing.T) {
	cfg := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}
	fc, sched, candidates, fallback := newRotationControllerTest(t, cfg)
	defer fc.Close()

	// Script an all-failure target-aware probe. The controller must probe the
	// current recovery target and report its result.
	var probeCalls []string
	var inFlight int
	var maxInFlight int
	fc.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		name := d.Property().Name
		probeCalls = append(probeCalls, name)
		// Simulate synchronous completion of a probe.
		defer func() { inFlight-- }()
		return false, nil
	}

	// Force the failure transition inline through the deterministic test seam.
	fc.triggerPrimaryFailureForTest()

	want := []struct {
		target string
		at     time.Duration
	}{
		{target: "A", at: 15 * time.Second},
		{target: "A", at: 45 * time.Second},
		{target: "A", at: 105 * time.Second},
		{target: "A", at: 225 * time.Second},
		{target: "A", at: 465 * time.Second},
	}

	for i, w := range want {
		// Verify the pending timer's scheduled deadline matches before firing.
		fc.mu.Lock()
		nextAt := fc.nextProbeAt
		fc.mu.Unlock()
		if got := nextAt.Sub(time.Unix(0, 0)); got != w.at {
			t.Fatalf("probe %d: scheduled deadline = %v, want %v", i, got, w.at)
		}
		sched.FireNext(t)
		if probeCalls[i] != w.target {
			t.Fatalf("probe %d target = %q, want %q", i, probeCalls[i], w.target)
		}
	}

	fc.mu.Lock()
	failedProbes := fc.failedRecoveryProbes
	rotationActive := fc.rotationActive
	recoveryTarget := fc.recoveryTarget
	currentPrimary := fc.currentPrimary
	nextAt := fc.nextProbeAt
	fc.mu.Unlock()

	if failedProbes != 0 {
		t.Fatalf("failedRecoveryProbes = %d, want 0 after advancement", failedProbes)
	}
	if !rotationActive {
		t.Fatal("rotationActive = false, want true")
	}
	if recoveryTarget != 1 {
		t.Fatalf("recoveryTarget = %d, want 1 (B)", recoveryTarget)
	}
	if currentPrimary != 0 {
		t.Fatalf("currentPrimary = %d, want 0 (A still current until promotion)", currentPrimary)
	}
	if got, _ := fc.ActiveDialer(); got != fallback {
		t.Fatalf("active dialer after rotation = %v, want fixed fallback", got)
	}
	if maxInFlight != 1 {
		t.Fatalf("max in-flight probes = %d, want 1", maxInFlight)
	}
	// The next probe is scheduled against B after ProbeInitial (15s) following
	// advancement. The backoff resets to initial on target advance.
	if got := nextAt.Sub(time.Unix(0, 0)); got != 480*time.Second {
		t.Fatalf("next probe deadline = %v, want 480s (B after reset initial)", got)
	}
	if got := sched.PendingCount(); got != 1 {
		t.Fatalf("pending timers after fifth failure = %d, want 1", got)
	}
	_ = candidates
}

// TestFailoverRotationCircularTargets verifies that after the threshold is
// reached, further failures advance recoveryTarget circularly through the
// ordered primary candidates B -> C -> A -> B (and would continue). The fixed
// fallback stays active throughout. Backoff stays capped at ProbeMax.
func TestFailoverRotationCircularTargets(t *testing.T) {
	cfg := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}
	fc, sched, _, fallback := newRotationControllerTest(t, cfg)
	defer fc.Close()

	var probeCalls []string
	fc.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		probeCalls = append(probeCalls, d.Property().Name)
		return false, nil
	}

	fc.triggerPrimaryFailureForTest()

	// Drive enough failed probes to observe the circular sequence:
	// 5 probes against A (exhausting the threshold), then B -> C -> A -> B.
	wantTargets := []string{
		"A", "A", "A", "A", "A", // threshold window on A
		"B", "B", "B", "B", "B", // 5 B failures
		"C", "C", "C", "C", "C", // 5 C failures
		"A", "A", "A", "A", "A", // 5 A failures
		"B", "B", "B", "B", // 4 B failures, not enough to advance
	}
	for range wantTargets {
		sched.FireNext(t)
	}

	if !reflect.DeepEqual(probeCalls, wantTargets) {
		t.Fatalf("probe targets = %v, want %v", probeCalls, wantTargets)
	}

	fc.mu.Lock()
	failedProbes := fc.failedRecoveryProbes
	rotationActive := fc.rotationActive
	recoveryTarget := fc.recoveryTarget
	currentDelay := fc.currentDelay
	fc.mu.Unlock()

	// 9 total failed probes (5 + 4). After the last B probe the cursor
	// advances to C (index 2) for the next probe.
	if failedProbes != 4 {
		t.Fatalf("failedRecoveryProbes = %d, want 4", failedProbes)
	}
	if !rotationActive {
		t.Fatal("rotationActive = false, want true")
	}
	if recoveryTarget != 1 {
		t.Fatalf("recoveryTarget = %d, want 1 (B)", recoveryTarget)
	}
	if currentDelay != 4*time.Minute {
		t.Fatalf("currentDelay = %v, want 4m (after 4 failures on B)", currentDelay)
	}
	if got, _ := fc.ActiveDialer(); got != fallback {
		t.Fatalf("active dialer = %v, want fixed fallback", got)
	}
}

// TestFailoverRotationProbeErrorMatchesFalseResult verifies that a non-cancellation
// probe error is treated exactly like ok=false: it consumes one consecutive
// attempt for the current target, clears confirmation state, and produces the
// same cursor and backoff result as a plain false result.
func TestFailoverRotationProbeErrorMatchesFalseResult(t *testing.T) {
	cfg := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}

	// errorResultFor maps a target name to whether its probe should return an
	// error instead of a plain false. The assertion is that the resulting
	// controller state is identical in both cases.
	type caseState struct {
		failedProbes   int
		rotationActive bool
		recoveryTarget int
		currentDelay   time.Duration
	}
	runCase := func(t *testing.T, errorResultFor map[string]bool) caseState {
		fc, sched, _, _ := newRotationControllerTest(t, cfg)
		defer fc.Close()

		fc.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
			name := d.Property().Name
			if errorResultFor[name] {
				return false, errors.New("probe error")
			}
			return false, nil
		}
		fc.triggerPrimaryFailureForTest()
		// Fire 6 probes: 5 against A (the fifth activates rotation and advances
		// to B), the 6th against B. With errorResultFor = {B: true}, that 6th
		// probe returns an error and must behave like a false result.
		for i := 0; i < 6; i++ {
			sched.FireNext(t)
		}
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return caseState{
			failedProbes:   fc.failedRecoveryProbes,
			rotationActive: fc.rotationActive,
			recoveryTarget: fc.recoveryTarget,
			currentDelay:   fc.currentDelay,
		}
	}

	falseCase := runCase(t, map[string]bool{})
	errCase := runCase(t, map[string]bool{"A": true, "B": true})

	if errCase != falseCase {
		t.Fatalf("error case %+v != false case %+v", errCase, falseCase)
	}
	if falseCase.failedProbes != 1 {
		t.Fatalf("failedRecoveryProbes = %d, want 1", falseCase.failedProbes)
	}
	if !falseCase.rotationActive {
		t.Fatal("rotationActive = false, want true")
	}
	if falseCase.recoveryTarget != 1 {
		t.Fatalf("recoveryTarget = %d, want 1 (B)", falseCase.recoveryTarget)
	}
	if falseCase.currentDelay != 30*time.Second {
		t.Fatalf("currentDelay = %v, want 30s (1st failure on B doubles from initial 15s)", falseCase.currentDelay)
	}
}

// TestFailoverRotationCancellationDoesNotConsumeFailure verifies that a
// context.Canceled returned by a probe (as happens during close/reload) is
// NOT treated as a failure: failedRecoveryProbes, rotationActive,
// recoveryTarget, confirmation state, and backoff remain unchanged.
func TestFailoverRotationCancellationDoesNotConsumeFailure(t *testing.T) {
	cfg := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}
	fc, sched, _, _ := newRotationControllerTest(t, cfg)
	defer fc.Close()

	// Make the probe return context.Canceled to simulate cancellation during
	// shutdown/reload.
	fc.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		return false, context.Canceled
	}
	fc.triggerPrimaryFailureForTest()

	// Snapshot state before the cancelled probe fires.
	fc.mu.Lock()
	beforeFailed := fc.failedRecoveryProbes
	beforeRotation := fc.rotationActive
	beforeTarget := fc.recoveryTarget
	beforeDelay := fc.currentDelay
	beforeSuccesses := fc.recoverySuccesses
	beforeStable := fc.stableSince
	fc.mu.Unlock()

	sched.FireNext(t)

	fc.mu.Lock()
	afterFailed := fc.failedRecoveryProbes
	afterRotation := fc.rotationActive
	afterTarget := fc.recoveryTarget
	afterDelay := fc.currentDelay
	afterSuccesses := fc.recoverySuccesses
	afterStable := fc.stableSince
	fc.mu.Unlock()

	if afterFailed != beforeFailed {
		t.Fatalf("failedRecoveryProbes changed on cancellation: %d -> %d", beforeFailed, afterFailed)
	}
	if afterRotation != beforeRotation {
		t.Fatalf("rotationActive changed on cancellation: %v -> %v", beforeRotation, afterRotation)
	}
	if afterTarget != beforeTarget {
		t.Fatalf("recoveryTarget changed on cancellation: %d -> %d", beforeTarget, afterTarget)
	}
	if afterDelay != beforeDelay {
		t.Fatalf("currentDelay changed on cancellation: %v -> %v", beforeDelay, afterDelay)
	}
	if afterSuccesses != beforeSuccesses {
		t.Fatalf("recoverySuccesses changed on cancellation: %d -> %d", beforeSuccesses, afterSuccesses)
	}
	if afterStable != beforeStable {
		t.Fatalf("stableSince changed on cancellation: %v -> %v", beforeStable, afterStable)
	}
}

// TestFailoverRotationRecoveryPromotion verifies the two promotion conditions:
// promotion requires BOTH recovery_successes consecutive successes AND
// recovery_stable_time elapsed since the first success. Confirmation probes
// run at ProbeInitial (not doubled). A failure resets confirmation (successes
// and stableSince) but does NOT erase the episode failure count. The cases
// drive the controller deterministically with the fake scheduler.
func TestFailoverRotationRecoveryPromotion(t *testing.T) {
	// ProbeInitial is small so confirmation probes are scheduled at +1s; the
	// test uses sched.Advance to pad elapsed time so the final success lands
	// at a precise offset from stableSince.
	baseCfg := FailoverRecoveryConfig{
		ProbeInitial:     1 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}

	// runRecovery builds a controller, triggers failover against A, then fires
	// one probe per result. Before each fire the clock is advanced by the
	// matching advance[i] so the final success occurs at a known offset from
	// the first success (stableSince). It returns the post-run state.
	type recoveryState struct {
		state             failoverState
		successes         int
		stableSinceIsZero bool
		currentPrimary    int
		rotationActive    bool
		failedProbes      int
		delay             time.Duration
		pendingTimers     int
	}
	runRecovery := func(t *testing.T, results []bool, advances []time.Duration) recoveryState {
		t.Helper()
		if len(advances) != len(results) {
			t.Fatalf("advances/results length mismatch: %d vs %d", len(advances), len(results))
		}
		fc, sched, _, _ := newRotationControllerTest(t, baseCfg)
		defer fc.Close()

		resultCh := make(chan bool, 1)
		fc.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
			// Probes against the recovery target return the next scripted
			// result. A success holds the cursor on that target; a failure
			// consumes one episode attempt and clears confirmation state.
			return <-resultCh, nil
		}
		fc.triggerPrimaryFailureForTest()

		for i, r := range results {
			resultCh <- r
			if advances[i] > 0 {
				sched.Advance(advances[i])
			}
			sched.FireNext(t)
		}

		fc.mu.Lock()
		defer fc.mu.Unlock()
		return recoveryState{
			state:             fc.state,
			successes:         fc.recoverySuccesses,
			stableSinceIsZero: fc.stableSince.IsZero(),
			currentPrimary:    fc.currentPrimary,
			rotationActive:    fc.rotationActive,
			failedProbes:      fc.failedRecoveryProbes,
			delay:             fc.currentDelay,
			pendingTimers:     sched.PendingCount(),
		}
	}

	tests := []struct {
		name          string
		results       []bool
		advances      []time.Duration
		wantPromoted  bool
		wantSuccesses int
	}{
		{
			name:          "three successes before stable time",
			results:       []bool{true, true, true},
			advances:      []time.Duration{0, 14 * time.Second, 15 * time.Second},
			wantPromoted:  false,
			wantSuccesses: 3,
		},
		{
			name:          "stable time with two successes",
			results:       []bool{true, true},
			advances:      []time.Duration{0, 29 * time.Second},
			wantPromoted:  false,
			wantSuccesses: 2,
		},
		{
			name:          "failure resets confirmation",
			results:       []bool{true, true, false, true},
			advances:      []time.Duration{0, 0, 0, 44 * time.Second},
			wantPromoted:  false,
			wantSuccesses: 1,
		},
		{
			name:          "both conditions",
			results:       []bool{true, true, true},
			advances:      []time.Duration{0, 0, 30 * time.Second},
			wantPromoted:  true,
			wantSuccesses: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runRecovery(t, tt.results, tt.advances)
			if tt.wantPromoted {
				if got.state != statePrimaryActive {
					t.Fatalf("state = %v, want statePrimaryActive (promoted)", got.state)
				}
				if got.currentPrimary != 0 {
					t.Fatalf("currentPrimary = %d, want 0 (A recovered)", got.currentPrimary)
				}
				if got.rotationActive {
					t.Fatal("rotationActive = true, want false after promotion")
				}
				if got.failedProbes != 0 {
					t.Fatalf("failedRecoveryProbes = %d, want 0 after promotion", got.failedProbes)
				}
				if got.successes != 0 {
					t.Fatalf("recoverySuccesses = %d, want 0 after promotion", got.successes)
				}
				if !got.stableSinceIsZero {
					t.Fatal("stableSince not zero after promotion")
				}
				if got.delay != baseCfg.ProbeInitial {
					t.Fatalf("currentDelay = %v, want ProbeInitial after promotion", got.delay)
				}
				if got.pendingTimers != 0 {
					t.Fatalf("pending timers = %d, want 0 (recovery schedule stopped)", got.pendingTimers)
				}
			} else {
				if got.state == statePrimaryActive {
					t.Fatalf("state = statePrimaryActive, want not promoted")
				}
				if got.successes != tt.wantSuccesses {
					t.Fatalf("recoverySuccesses = %d, want %d", got.successes, tt.wantSuccesses)
				}
				// Still on fallback: no promotion happened.
				if got.currentPrimary != 0 {
					t.Fatalf("currentPrimary = %d, want 0 (unchanged)", got.currentPrimary)
				}
				if got.pendingTimers != 1 {
					t.Fatalf("pending timers = %d, want 1 (recovery schedule running)", got.pendingTimers)
				}
			}
		})
	}
}

// findLogEntries returns all log entries whose message matches one of the
// supplied names.
func findLogEntries(hook *logrustest.Hook, messages ...string) []*logrus.Entry {
	want := map[string]struct{}{}
	for _, m := range messages {
		want[m] = struct{}{}
	}
	var out []*logrus.Entry
	for _, e := range hook.AllEntries() {
		if _, ok := want[e.Message]; ok {
			out = append(out, e)
		}
	}
	return out
}

// TestFailoverRotationStructuredLogs verifies the structured
// primary_rotation_started and recovery_target_advanced logs. Both are
// debug-level and carry group, failed_primary, from_target, to_target,
// failed_attempts, and next_probe_in. failed_primary stays A across the whole
// episode; failed_attempts is the exhausted per-target consecutive budget;
// from_target/to_target report the cursor edge; next_probe_in is the actual
// scheduled delay.
func TestFailoverRotationStructuredLogs(t *testing.T) {
	logger, hook := logrustest.NewNullLogger()
	logger.SetLevel(logrus.DebugLevel)

	cfg := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}
	fc, sched, candidates, _ := newRotationControllerWithLogger(t, logger, cfg)
	defer fc.Close()

	fc.probeTargetTCP = func(_ context.Context, d *dialer.Dialer) (bool, error) {
		// All probes fail; the cursor advances through A -> B -> C -> A.
		_ = d
		return false, nil
	}
	fc.triggerPrimaryFailureForTest()

	// Fire 20 probes: 5 A, 5 B, 5 C, 5 A.
	for i := 0; i < 20; i++ {
		sched.FireNext(t)
	}

	started := findLogEntries(hook, "primary_rotation_started")
	if len(started) != 1 {
		t.Fatalf("primary_rotation_started emitted %d times, want 1", len(started))
	}
	if started[0].Level != logrus.DebugLevel {
		t.Fatalf("primary_rotation_started level = %v, want Debug", started[0].Level)
	}
	checkRotationFields(t, started[0], "A", "A", "B", 5, 15*time.Second)

	advanced := findLogEntries(hook, "recovery_target_advanced")
	if len(advanced) != 3 {
		t.Fatalf("recovery_target_advanced emitted %d times, want 3", len(advanced))
	}
	checkRotationFields(t, advanced[0], "A", "B", "C", 5, 15*time.Second)
	checkRotationFields(t, advanced[1], "A", "C", "A", 5, 15*time.Second)
	checkRotationFields(t, advanced[2], "A", "A", "B", 5, 15*time.Second)
	_ = candidates
}

func TestNextRecoveryProbeDelay(t *testing.T) {
	base := FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
	}
	tests := []struct {
		name     string
		backoff  FailoverProbeBackoff
		current  time.Duration
		advanced bool
		want     time.Duration
	}{
		{"fixed same target", FailoverProbeBackoffFixed, 4 * time.Minute, false, 15 * time.Second},
		{"fixed advanced", FailoverProbeBackoffFixed, 4 * time.Minute, true, 15 * time.Second},
		{"exponential growth", FailoverProbeBackoffExponential, time.Minute, false, 2 * time.Minute},
		{"exponential cap crossing", FailoverProbeBackoffExponential, 4 * time.Minute, false, 5 * time.Minute},
		{"exponential remains capped", FailoverProbeBackoffExponential, 5 * time.Minute, false, 5 * time.Minute},
		{"exponential advanced", FailoverProbeBackoffExponential, 5 * time.Minute, true, 15 * time.Second},
		{"overflow safe", FailoverProbeBackoffExponential, time.Duration(1<<63 - 2), false, 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.Backoff = tt.backoff
			require.Equal(t, tt.want, nextRecoveryProbeDelay(tt.current, cfg, tt.advanced))
		})
	}
}

func TestFailoverRotationFixedModePacingAndConfirmation(t *testing.T) {
	cfg := FailoverRecoveryConfig{
		Backoff:          FailoverProbeBackoffFixed,
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}
	fc, sched, _, fallback := newRotationControllerTest(t, cfg)
	defer fc.Close()

	var probeCalls []string
	var probeTimes []time.Duration
	fc.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		probeCalls = append(probeCalls, d.Property().Name)
		probeTimes = append(probeTimes, sched.Now().Sub(time.Unix(0, 0)))
		return false, nil
	}

	fc.triggerPrimaryFailureForTest()

	// Drive 10 failures: 5 on A, 5 on B.
	for i := 0; i < 10; i++ {
		sched.FireNext(t)
	}

	wantTargets := []string{
		"A", "A", "A", "A", "A",
		"B", "B", "B", "B", "B",
	}
	require.Equal(t, wantTargets, probeCalls)

	// Assert every delta in scheduler history is 15 seconds.
	for i := 1; i < len(probeTimes); i++ {
		delta := probeTimes[i] - probeTimes[i-1]
		require.Equal(t, 15*time.Second, delta, "probe %d delta", i)
	}

	require.Equal(t, 1, sched.PendingCount())
	got, _ := fc.ActiveDialer()
	require.Equal(t, fallback, got)
}

func TestFailoverRotationConfirmationFailurePacing(t *testing.T) {
	t.Run("fixed mode confirmation failure", func(t *testing.T) {
		cfg := FailoverRecoveryConfig{
			Backoff:          FailoverProbeBackoffFixed,
			ProbeInitial:     15 * time.Second,
			ProbeMax:         5 * time.Minute,
			Successes:        3,
			StableTime:       30 * time.Second,
			RotationAttempts: 5,
		}
		fc, sched, _, _ := newRotationControllerTest(t, cfg)
		defer fc.Close()

		var returnSuccess bool
		fc.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
			return returnSuccess, nil
		}
		fc.triggerPrimaryFailureForTest()

		// First probe succeeds: enters confirmation
		returnSuccess = true
		sched.FireNext(t)

		fc.mu.Lock()
		require.Equal(t, 1, fc.recoverySuccesses)
		require.Equal(t, 15*time.Second, fc.currentDelay)
		fc.mu.Unlock()

		// Next probe (confirmation probe) fails: clears confirmation, stays at 15s in fixed mode
		returnSuccess = false
		sched.FireNext(t)

		fc.mu.Lock()
		require.Equal(t, 0, fc.recoverySuccesses)
		require.Equal(t, 15*time.Second, fc.currentDelay)
		fc.mu.Unlock()
	})

	t.Run("exponential mode confirmation failure", func(t *testing.T) {
		cfg := FailoverRecoveryConfig{
			Backoff:          FailoverProbeBackoffExponential,
			ProbeInitial:     15 * time.Second,
			ProbeMax:         5 * time.Minute,
			Successes:        3,
			StableTime:       30 * time.Second,
			RotationAttempts: 5,
		}
		fc, sched, _, _ := newRotationControllerTest(t, cfg)
		defer fc.Close()

		var returnSuccess bool
		fc.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
			return returnSuccess, nil
		}
		fc.triggerPrimaryFailureForTest()

		// First probe succeeds: enters confirmation, currentDelay reset to ProbeInitial (15s)
		returnSuccess = true
		sched.FireNext(t)

		// Next probe (confirmation probe) fails: clears confirmation, doubles delay to 30s
		returnSuccess = false
		sched.FireNext(t)

		fc.mu.Lock()
		require.Equal(t, 0, fc.recoverySuccesses)
		require.Equal(t, 30*time.Second, fc.currentDelay)
		fc.mu.Unlock()
	})
}

// checkRotationFields asserts the structured rotation log fields.
func checkRotationFields(t *testing.T, e *logrus.Entry, failedPrimary, fromTarget, toTarget string, failedAttempts int, nextProbeIn time.Duration) {
	t.Helper()
	if got := e.Data["group"]; got != "rotation-test" {
		t.Fatalf("group = %v, want rotation-test", got)
	}
	if got := e.Data["failed_primary"]; got != failedPrimary {
		t.Fatalf("failed_primary = %v, want %s", got, failedPrimary)
	}
	if got := e.Data["from_target"]; got != fromTarget {
		t.Fatalf("from_target = %v, want %s", got, fromTarget)
	}
	if got := e.Data["to_target"]; got != toTarget {
		t.Fatalf("to_target = %v, want %s", got, toTarget)
	}
	if got := e.Data["failed_attempts"]; got != failedAttempts {
		t.Fatalf("failed_attempts = %v, want %d", got, failedAttempts)
	}
	got, ok := e.Data["next_probe_in"].(time.Duration)
	if !ok {
		t.Fatalf("next_probe_in type = %T, want time.Duration", e.Data["next_probe_in"])
	}
	if got != nextProbeIn {
		t.Fatalf("next_probe_in = %v, want %v", got, nextProbeIn)
	}
}

// TestFailoverRotationDynamicEventPrimary verifies that failover_switch and
// failback_complete carry the actual dynamic current primary name in the
// Primary field and the fixed fallback name in the Fallback field. Routine
// candidate movement does NOT emit additional FailoverEvents.
func TestFailoverRotationDynamicEventPrimary(t *testing.T) {
	logger, _ := logrustest.NewNullLogger()
	logger.SetLevel(logrus.DebugLevel)

	cfg := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}
	fc, sched, candidates, fallback := newRotationControllerWithLogger(t, logger, cfg)
	defer fc.Close()
	_ = fallback
	_ = candidates

	cb := &recordingCallback{}
	fc.SetEventCallback(cb)

	// Script: 5 A failures (activates rotation, advances to B), then 3 B
	// successes at 15s cadence promote B. The probe results alternate by
	// target identity.
	fc.probeTargetTCP = func(_ context.Context, d *dialer.Dialer) (bool, error) {
		switch d.Property().Name {
		case "A":
			return false, nil
		case "B":
			return true, nil
		default:
			return false, nil
		}
	}
	fc.triggerPrimaryFailureForTest()

	// Drive: 5 A failures, then 3 B successes. The stable-time check uses
	// sched.Now(); FireNext advances the clock to each timer's scheduled
	// deadline. After the fifth A failure the cursor moves to B and the next
	// probe is scheduled at +5m. The first B success records stableSince at
	// that 5m offset; the next two B confirmation probes run at +15s each,
	// so the third success lands at +5m30s — exactly 30s after stableSince.
	for i := 0; i < 5; i++ {
		sched.FireNext(t)
	}
	for i := 0; i < 3; i++ {
		sched.FireNext(t)
	}

	events := cb.snapshot()
	if len(events) != 2 {
		t.Fatalf("emitted %d events, want 2 (switch + failback)", len(events))
	}
	if events[0].Type != FailoverEventSwitch {
		t.Fatalf("event 0 type = %v, want failover_switch", events[0].Type)
	}
	if events[0].Primary != "A" {
		t.Fatalf("switch event Primary = %q, want A (dynamic current primary)", events[0].Primary)
	}
	if events[0].Fallback != "fallback" {
		t.Fatalf("switch event Fallback = %q, want fallback", events[0].Fallback)
	}
	if events[1].Type != FailoverEventFailbackComplete {
		t.Fatalf("event 1 type = %v, want failback_complete", events[1].Type)
	}
	if events[1].Primary != "B" {
		t.Fatalf("failback event Primary = %q, want B (promoted candidate)", events[1].Primary)
	}
	if events[1].Fallback != "fallback" {
		t.Fatalf("failback event Fallback = %q, want fallback", events[1].Fallback)
	}
	if got, _ := fc.ActiveDialer(); got != candidates[1] {
		t.Fatalf("active dialer = %v, want promoted B", got)
	}
}

// TestFailoverRotationNotifierIsolation verifies that a notifier (Bark
// dispatch) failure does not change selection, counters, cursor, timers, or
// the subsequent promotion. The state machine must remain unaffected by
// notifier errors. The controller's own emitEventLocked recover is the
// production safety net, so this test installs a panicking callback that
// records events AND panics — the recording must still observe every event
// and the state machine must match a no-op-notifier run.
func TestFailoverRotationNotifierIsolation(t *testing.T) {
	logger, _ := logrustest.NewNullLogger()
	logger.SetLevel(logrus.DebugLevel)

	cfg := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}

	// runWithNotifier builds a controller, installs the supplied event
	// callback, drives the same probe script, and returns the post-run state.
	type isolatedState struct {
		activeName     string
		currentPrimary int
		failedProbes   int
		recoveryTarget int
		pendingTimers  int
		promoted       bool
		switchEvents   int
		failbackEvents int
	}
	runWithCallback := func(t *testing.T, cb FailoverEventCallback) isolatedState {
		t.Helper()
		fc, sched, _, _ := newRotationControllerWithLogger(t, logger, cfg)
		defer fc.Close()
		fc.SetEventCallback(cb)

		fc.probeTargetTCP = func(_ context.Context, d *dialer.Dialer) (bool, error) {
			switch d.Property().Name {
			case "A":
				return false, nil
			case "B":
				return true, nil
			default:
				return false, nil
			}
		}
		fc.triggerPrimaryFailureForTest()
		for i := 0; i < 5; i++ {
			sched.FireNext(t)
		}
		for i := 0; i < 3; i++ {
			sched.FireNext(t)
		}

		fc.mu.Lock()
		defer fc.mu.Unlock()
		active, _ := fc.ActiveDialer()
		activeName := "<nil>"
		if active != nil {
			activeName = active.Property().Name
		}
		var switchEvents, failbackEvents int
		switch recorder := cb.(type) {
		case *recordingCallback:
			events := recorder.snapshot()
			for _, ev := range events {
				switch ev.Type {
				case FailoverEventSwitch:
					switchEvents++
				case FailoverEventFailbackComplete:
					failbackEvents++
				}
			}
		case *panickingCallback:
			events := recorder.inner.snapshot()
			for _, ev := range events {
				switch ev.Type {
				case FailoverEventSwitch:
					switchEvents++
				case FailoverEventFailbackComplete:
					failbackEvents++
				}
			}
		}
		return isolatedState{
			activeName:     activeName,
			currentPrimary: fc.currentPrimary,
			failedProbes:   fc.failedRecoveryProbes,
			recoveryTarget: fc.recoveryTarget,
			pendingTimers:  sched.PendingCount(),
			promoted:       fc.state == statePrimaryActive,
			switchEvents:   switchEvents,
			failbackEvents: failbackEvents,
		}
	}

	noopState := runWithCallback(t, &recordingCallback{})

	// panickingRecorder records the event then panics. The controller's
	// emitEventLocked recover must catch the panic so the state machine is
	// unaffected. The recording still observes every event because
	// OnFailoverEvent appends before panicking.
	panickingRecorder := &recordingCallback{}
	panicState := runWithCallback(t, &panickingCallback{inner: panickingRecorder})

	if !reflect.DeepEqual(noopState, panicState) {
		t.Fatalf("notifier failure changed state machine:\nnoop=%+v\npanic=%+v", noopState, panicState)
	}
	if panicState.promoted != true {
		t.Fatalf("promoted = %v, want true (notifier failure must not block promotion)", panicState.promoted)
	}
	if panicState.activeName != "B" {
		t.Fatalf("active = %s, want B (notifier failure must not change selection)", panicState.activeName)
	}
	if panicState.switchEvents != 1 || panicState.failbackEvents != 1 {
		t.Fatalf("events = switch=%d failback=%d, want 1/1", panicState.switchEvents, panicState.failbackEvents)
	}
}

// panickingCallback records the event via the inner recording callback and
// then panics, simulating a Bark dispatch failure. The controller's
// emitEventLocked recover catches the panic so the state machine is
// unaffected; the recording still observes every event.
type panickingCallback struct {
	inner *recordingCallback
}

func (p *panickingCallback) OnFailoverEvent(ev FailoverEvent) {
	p.inner.OnFailoverEvent(ev)
	panic("bark dispatch failed")
}

func TestFailoverRotationUsesPerCandidateConsecutiveBudget(t *testing.T) {
	cfg := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 2,
	}
	fc, sched, candidates, _ := newRotationControllerTest(t, cfg)
	defer fc.Close()
	results := []struct {
		target *dialer.Dialer
		ok     bool
	}{
		{target: candidates[0]},
		{target: candidates[0]},
		{target: candidates[1]},
		{target: candidates[1], ok: true},
		{target: candidates[1]},
		{target: candidates[1]},
	}
	next := 0
	fc.probeTargetTCP = func(_ context.Context, target *dialer.Dialer) (bool, error) {
		result := results[next]
		next++
		if target != result.target {
			t.Fatalf("probe target = %s, want %s", target.Property().Name, result.target.Property().Name)
		}
		return result.ok, nil
	}
	fc.triggerPrimaryFailureForTest()
	for range results {
		sched.FireNext(t)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.recoveryTarget != 2 {
		t.Fatalf("recoveryTarget = %d, want 2 (C)", fc.recoveryTarget)
	}
	if fc.failedRecoveryProbes != 0 {
		t.Fatalf("failedRecoveryProbes = %d, want 0 after advancement", fc.failedRecoveryProbes)
	}
}

func TestFailoverRotationFiveConsecutiveFailuresPerCandidate(t *testing.T) {
	cfg := FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}
	fc, sched, candidates, fallback := newRotationControllerTest(t, cfg)
	defer fc.Close()

	type probeResult struct {
		target *dialer.Dialer
		ok     bool
	}
	results := []probeResult{
		{target: candidates[0]},
		{target: candidates[0]},
		{target: candidates[0]},
		{target: candidates[0]},
		{target: candidates[0], ok: true},
		{target: candidates[0]},
		{target: candidates[0]},
		{target: candidates[0]},
		{target: candidates[0]},
		{target: candidates[0]},
		{target: candidates[1]},
		{target: candidates[1]},
		{target: candidates[1]},
		{target: candidates[1]},
		{target: candidates[1]},
	}
	wantTargets := []string{
		"A", "A", "A", "A", "A",
		"A", "A", "A", "A", "B",
		"B", "B", "B", "B", "C",
	}
	wantFailures := []int{
		1, 2, 3, 4, 0,
		1, 2, 3, 4, 0,
		1, 2, 3, 4, 0,
	}

	next := 0
	fc.probeTargetTCP = func(_ context.Context, target *dialer.Dialer) (bool, error) {
		result := results[next]
		next++
		require.Same(t, result.target, target)
		return result.ok, nil
	}
	fc.triggerPrimaryFailureForTest()

	for i := range results {
		sched.FireNext(t)
		fc.mu.Lock()
		target := fc.recoveryTargetNameLocked()
		failures := fc.failedRecoveryProbes
		fc.mu.Unlock()
		require.Equalf(t, wantTargets[i], target, "probe %d recovery target", i+1)
		require.Equalf(t, wantFailures[i], failures, "probe %d consecutive failures", i+1)
		active, usingFallback := fc.ActiveDialer()
		require.Same(t, fallback, active)
		require.True(t, usingFallback)
	}
	require.Equal(t, len(results), next)
}

func TestFailoverRotationCandidateCountAndZeroThreshold(t *testing.T) {
	tests := []struct {
		name         string
		candidates   []string
		attempts     int
		failures     int
		wantTarget   int
		wantFailures int
	}{
		{"one candidate positive", []string{"A"}, 2, 2, 0, 0},
		{"one candidate disabled", []string{"A"}, 0, 6, 0, 6},
		{"three candidates disabled", []string{"A", "B", "C"}, 0, 6, 0, 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := FailoverRecoveryConfig{
				ProbeInitial:     15 * time.Second,
				ProbeMax:         5 * time.Minute,
				Successes:        3,
				StableTime:       30 * time.Second,
				RotationAttempts: tt.attempts,
			}

			option := testFailoverDialerOption()
			var candidates []*dialer.Dialer
			for _, n := range tt.candidates {
				candidates = append(candidates, newNamedDirectDialer(option, n))
			}
			fallback := newNamedDirectDialer(option, "fallback")

			fc := NewFailoverControllerWithCandidates(log, "rotation-test", candidates, fallback, cfg)
			sched := newFakeFailoverScheduler()
			fc.scheduler = sched
			defer fc.Close()

			fc.probeTargetTCP = func(_ context.Context, target *dialer.Dialer) (bool, error) {
				return false, nil
			}

			fc.triggerPrimaryFailureForTest()

			for i := 0; i < tt.failures; i++ {
				sched.FireNext(t)
				if got, _ := fc.ActiveDialer(); got != fallback {
					t.Fatalf("active dialer = %v, want fallback", got)
				}
			}

			fc.mu.Lock()
			defer fc.mu.Unlock()

			if fc.recoveryTarget != tt.wantTarget {
				t.Fatalf("recoveryTarget = %d, want %d", fc.recoveryTarget, tt.wantTarget)
			}
			if fc.failedRecoveryProbes != tt.wantFailures {
				t.Fatalf("failedRecoveryProbes = %d, want %d", fc.failedRecoveryProbes, tt.wantFailures)
			}
		})
	}
}

func TestFailoverRotationZeroThresholdStillRecovers(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
	}{
		{name: "one candidate", candidates: []string{"A"}},
		{name: "three candidates", candidates: []string{"A", "B", "C"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := FailoverRecoveryConfig{
				ProbeInitial:     15 * time.Second,
				ProbeMax:         5 * time.Minute,
				Successes:        2,
				StableTime:       15 * time.Second,
				RotationAttempts: 0,
			}
			option := testFailoverDialerOption()
			primaryCandidates := make([]*dialer.Dialer, 0, len(tt.candidates))
			for _, name := range tt.candidates {
				primaryCandidates = append(primaryCandidates, newNamedDirectDialer(option, name))
			}
			fallback := newNamedDirectDialer(option, "fallback")
			fc := NewFailoverControllerWithCandidates(log, "zero-threshold", primaryCandidates, fallback, cfg)
			sched := newFakeFailoverScheduler()
			fc.scheduler = sched
			defer fc.Close()

			results := []bool{false, false, false, false, false, false, true, true}
			next := 0
			fc.probeTargetTCP = func(_ context.Context, target *dialer.Dialer) (bool, error) {
				require.Same(t, primaryCandidates[0], target)
				result := results[next]
				next++
				return result, nil
			}
			fc.triggerPrimaryFailureForTest()

			for range 6 {
				sched.FireNext(t)
				fc.mu.Lock()
				recoveryTarget := fc.recoveryTarget
				fc.mu.Unlock()
				require.Zero(t, recoveryTarget)
				active, usingFallback := fc.ActiveDialer()
				require.Same(t, fallback, active)
				require.True(t, usingFallback)
				require.Equal(t, 1, sched.PendingCount())
			}
			require.Len(t, sched.history, 7)
			wantFailureSchedule := []time.Duration{
				15 * time.Second,
				30 * time.Second,
				1 * time.Minute,
				2 * time.Minute,
				4 * time.Minute,
				5 * time.Minute,
				5 * time.Minute,
			}
			previous := time.Unix(0, 0)
			for i, deadline := range sched.history {
				require.Equalf(t, wantFailureSchedule[i], deadline.Sub(previous), "scheduled delay %d", i+1)
				previous = deadline
			}

			sched.FireNext(t)
			active, usingFallback := fc.ActiveDialer()
			require.Same(t, fallback, active, "partial confirmation must keep fallback active")
			require.True(t, usingFallback)
			require.Equal(t, 1, sched.PendingCount())
			require.Len(t, sched.history, 8)
			require.Equal(t, cfg.ProbeInitial, sched.history[7].Sub(sched.history[6]))

			sched.FireNext(t)
			active, usingFallback = fc.ActiveDialer()
			require.Same(t, primaryCandidates[0], active)
			require.False(t, usingFallback)
			require.Equal(t, len(results), next)
			require.Zero(t, sched.PendingCount())
		})
	}
}
