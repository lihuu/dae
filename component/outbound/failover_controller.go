/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daeuniverse/dae/component/outbound/dialer"
	"github.com/sirupsen/logrus"
)

// failoverTimer is the minimal timer interface the controller depends on so a
// fake scheduler can intercept scheduling in tests. time.Timer and the fake
// timer both satisfy it.
type failoverTimer interface {
	Stop() bool
}

// failoverScheduler abstracts the controller's time source and one-shot
// timer scheduling so the rotation state-machine tests can drive the recovery
// loop deterministically without real-time sleeps. The production
// implementation is systemFailoverScheduler; tests inject a fake.
type failoverScheduler interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) failoverTimer
}

// systemFailoverScheduler wraps the real time package. It is the default
// scheduler so legacy tests that rely on real time.AfterFunc keep working
// unchanged.
type systemFailoverScheduler struct{}

func (systemFailoverScheduler) Now() time.Time { return time.Now() }
func (systemFailoverScheduler) AfterFunc(d time.Duration, fn func()) failoverTimer {
	return time.AfterFunc(d, fn)
}

// minDuration returns the smaller of two durations.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// failoverState represents the logical state of the failover group.
type failoverState int

const (
	// statePrimaryActive: primary is healthy, used for all new connections.
	statePrimaryActive failoverState = iota
	// stateFallbackActive: primary failed, fallback is active, probing primary.
	stateFallbackActive
	// stateRecovering: primary probe succeeded, confirming stability.
	stateRecovering
)

// failoverSnapshot is an immutable snapshot of the failover controller state,
// stored atomically for lock-free reads on the hot selection path.
//
// activeDialer is the dialer pointer the hot path must return. usingFallback
// is true only when the fixed Fallback role is active (states stateFallbackActive
// and stateRecovering). When the current Primary is active, usingFallback is
// false and activeDialer is primaryCandidates[currentPrimary].
type failoverSnapshot struct {
	state         failoverState
	activeDialer  *dialer.Dialer
	usingFallback bool
}

// FailoverRecoveryConfig holds the recovery probe parameters.
type FailoverRecoveryConfig struct {
	ProbeInitial     time.Duration
	ProbeMax         time.Duration
	Successes        int
	StableTime       time.Duration
	RotationAttempts int
}

// FailoverController manages the failover state machine for a DialerGroup.
// It observes the primary dialer's TCP health transitions and drives recovery
// probing with exponential backoff.
type FailoverController struct {
	log       *logrus.Logger
	groupName string

	// primaryCandidates is the ordered list of primary candidates, sorted by
	// numeric priority ascending. Index 0 is the initial current primary.
	primaryCandidates []*dialer.Dialer
	// currentPrimary is the index into primaryCandidates of the dialer that
	// serves new traffic during normal operation. Initialized to 0 on a fresh
	// controller. Rotation advances this index (Packet 3).
	currentPrimary int
	// recoveryTarget is the index into primaryCandidates of the dialer the
	// recovery probe is currently targeting. Before rotation activates it is
	// the failed current primary; after rotation it advances through the
	// candidates. Initialized to 0 on a fresh controller (Packet 3 drives the
	// actual advancement).
	recoveryTarget int
	fallback       *dialer.Dialer
	config         FailoverRecoveryConfig
	probeTCP       func(context.Context) (bool, error)
	// probeTargetTCP, when set, overrides probeTCP for target-aware recovery
	// probes. It receives the dialer of the current recoveryTarget. When nil,
	// the controller falls back to probeTCP against the current target dialer
	// (the legacy single-primary path). Tests inject it to script per-target
	// results deterministically.
	probeTargetTCP func(context.Context, *dialer.Dialer) (bool, error)
	// scheduler is the time/timer source. It defaults to
	// systemFailoverScheduler so production and legacy tests use real time;
	// rotation tests inject a fakeFailoverScheduler for deterministic timing.
	scheduler failoverScheduler

	// mu protects mutable state below. It must NOT be held during network ops.
	mu sync.Mutex

	state             failoverState
	recoverySuccesses int
	stableSince       time.Time // when the first recovery success occurred
	currentDelay      time.Duration
	recoveryTimer     failoverTimer
	nextProbeAt       time.Time          // when the current recovery timer is scheduled to fire
	probeCancel       context.CancelFunc // cancels the in-flight probe
	generation        uint64             // incremented on close/reload to invalidate stale callbacks

	// rotationActive is true once failedRecoveryProbes has reached
	// config.RotationAttempts and the recoveryTarget has begun advancing
	// through the ordered primary candidates. Before that, failed probes
	// keep targeting the failed current primary.
	rotationActive bool
	// failedRecoveryProbes is the monotonically increasing total failed
	// recovery-probe count during the current failover episode. A successful
	// probe does NOT reset it. It resets only when a candidate completes
	// stable recovery and is promoted, or on a fresh controller without an
	// inherited snapshot.
	failedRecoveryProbes int
	// probeInFlight tracks whether a recovery probe is currently running. At
	// most one probe is in flight at any time; scheduling a new probe while
	// one is running is prevented by the generation guard and this flag.
	probeInFlight bool

	// closed is set by Close(). After closed, onPrimaryHealthChange returns
	// early without modifying state or scheduling timers.
	closed bool

	// eventCallback receives transition events. May be nil (notifications
	// disabled). Set via SetEventCallback before traffic starts. The callback
	// must be non-blocking (queue-send only) because it is invoked while mu is
	// held.
	eventCallback FailoverEventCallback

	// snapshot is read lock-free on the selection path.
	snapshot atomic.Pointer[failoverSnapshot]
}

// NewFailoverControllerWithCandidates creates a failover controller with an
// ordered list of primary candidates and a fixed fallback. The first candidate
// (primaryCandidates[0]) is the initial current primary. The remaining
// candidates are standby primaries reserved for rotation (Packet 3).
//
// This constructor stores the structural fields only; it does not wire rotation
// state-machine logic or standby health callbacks. The current Primary's TCP
// health transition callback is registered so the existing failover transition
// continues to fire.
func NewFailoverControllerWithCandidates(
	log *logrus.Logger,
	groupName string,
	primaryCandidates []*dialer.Dialer,
	fallback *dialer.Dialer,
	config FailoverRecoveryConfig,
) *FailoverController {
	if config.ProbeInitial <= 0 {
		config.ProbeInitial = 15 * time.Second
	}
	if config.ProbeMax <= 0 {
		config.ProbeMax = 5 * time.Minute
	}
	if config.Successes <= 0 {
		config.Successes = 3
	}
	if config.StableTime <= 0 {
		config.StableTime = 30 * time.Second
	}

	if len(primaryCandidates) == 0 {
		// Defensive: callers validate this, but guard against a nil primary to
		// keep the hot path's nil check meaningful.
		primaryCandidates = []*dialer.Dialer{nil}
	}

	primary := primaryCandidates[0]

	fc := &FailoverController{
		log:               log,
		groupName:         groupName,
		primaryCandidates: primaryCandidates,
		currentPrimary:    0,
		recoveryTarget:    0,
		fallback:          fallback,
		config:            config,
		probeTCP:          primary.ProbeTCPOnce,
		scheduler:         systemFailoverScheduler{},
		state:             statePrimaryActive,
		currentDelay:      config.ProbeInitial,
	}
	fc.publishSnapshot()

	// Register for the current primary's TCP health transitions.
	primary.RegisterAliveTransitionCallback(fc.onPrimaryHealthChange)
	// Keep the current primary's connectivity check goroutine alive so that
	// traffic-driven failures are detected and the above callback fires.
	primary.MarkKeepConnectivityCheck()

	return fc
}

// NewFailoverController creates a failover controller with a single primary
// and a fixed fallback. It is a legacy adapter that delegates to
// NewFailoverControllerWithCandidates. Existing tests and callers that do not
// use primary rotation continue to work through this constructor.
func NewFailoverController(
	log *logrus.Logger,
	groupName string,
	primary *dialer.Dialer,
	fallback *dialer.Dialer,
	config FailoverRecoveryConfig,
) *FailoverController {
	return NewFailoverControllerWithCandidates(log, groupName, []*dialer.Dialer{primary}, fallback, config)
}

// SetEventCallback installs a failover event callback. It must be called
// before traffic starts (i.e., immediately after construction). The callback
// must be non-blocking because OnFailoverEvent is invoked while the
// controller mutex is held.
func (fc *FailoverController) SetEventCallback(cb FailoverEventCallback) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.eventCallback = cb
}

// ActiveDialer returns the currently active dialer pointer and whether the
// fixed Fallback role is active. This is the lock-free hot path: a single
// atomic snapshot load, no candidate traversal, no mutex, no health work.
// The boolean is true only when the fixed Fallback is active (states
// stateFallbackActive and stateRecovering).
func (fc *FailoverController) ActiveDialer() (*dialer.Dialer, bool) {
	snap := fc.snapshot.Load()
	return snap.activeDialer, snap.usingFallback
}

// ActiveDialerIndex returns a legacy role index for test/caller adapter
// compatibility: 0 for the current Primary, 1 for the fixed Fallback. New code
// should prefer ActiveDialer for the dialer pointer and usingFallback flag.
func (fc *FailoverController) ActiveDialerIndex() int {
	snap := fc.snapshot.Load()
	if snap.usingFallback {
		return 1
	}
	return 0
}

// primaryDialer returns the current primary dialer. Must be called with mu held.
func (fc *FailoverController) primaryDialer() *dialer.Dialer {
	if fc.currentPrimary < 0 || fc.currentPrimary >= len(fc.primaryCandidates) {
		return nil
	}
	return fc.primaryCandidates[fc.currentPrimary]
}

// State returns the current failover state (for testing).
func (fc *FailoverController) State() failoverState {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.state
}

// onPrimaryHealthChange is called when the primary dialer's TCP health transitions.
func (fc *FailoverController) onPrimaryHealthChange(networkType *dialer.NetworkType, alive bool) {
	// Only TCP transitions trigger failover.
	if networkType.L4Proto != "tcp" {
		return
	}

	if alive {
		// Primary recovered — handled by recovery probe, not here.
		// Real traffic success during fallback_active/ recovering would be
		// observed by the probe. We don't switch back on a single success.
		return
	}

	// Primary TCP became unavailable.
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.closed || fc.state != statePrimaryActive {
		// Already in fallback or recovering; ignore duplicate.
		return
	}

	fc.startFailureTransitionLocked("tcp_unavailable")
}

// startFailureTransitionLocked performs the Failure Transition for the current
// primary: selects the fixed fallback for new traffic, sets recoveryTarget to
// the failed current primary, resets rotationActive and failedRecoveryProbes,
// resets the backoff, schedules the first probe, and emits failover_switch.
// Must hold mu. Called from the production health callback (Packet 4 wires
// that to candidate transitions) and the test seam triggerPrimaryFailureForTest.
func (fc *FailoverController) startFailureTransitionLocked(trigger string) {
	fc.log.WithFields(logrus.Fields{
		"group":    fc.groupName,
		"from":     "primary",
		"to":       "fallback",
		"trigger":  trigger,
		"primary":  dialerName(fc.primaryDialer()),
		"fallback": dialerName(fc.fallback),
	}).Info("failover_switch")

	fc.state = stateFallbackActive
	fc.recoveryTarget = fc.currentPrimary
	fc.rotationActive = false
	fc.failedRecoveryProbes = 0
	fc.recoverySuccesses = 0
	fc.stableSince = time.Time{}
	fc.currentDelay = fc.config.ProbeInitial
	fc.publishSnapshot()
	fc.emitEventLocked(FailoverEvent{
		Type:         FailoverEventSwitch,
		Group:        fc.groupName,
		From:         "primary",
		To:           "fallback",
		Primary:      dialerName(fc.primaryDialer()),
		Fallback:     dialerName(fc.fallback),
		Trigger:      trigger,
		TransitionAt: fc.scheduler.Now(),
	})

	// Schedule the first recovery probe.
	fc.scheduleProbeLocked()
}

// triggerPrimaryFailureForTest is a test-only seam that performs the Failure
// Transition inline against the current primary without going through the
// alive-transition callback machinery. Packet 4 wires the real health
// callbacks; this method exists so the rotation state machine (Packet 3) can
// be exercised deterministically before that wiring exists. Do NOT call from
// production code.
func (fc *FailoverController) triggerPrimaryFailureForTest() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.state != statePrimaryActive {
		return
	}
	fc.startFailureTransitionLocked("tcp_unavailable")
}

// scheduleProbeLocked schedules the next recovery probe. Must be called with mu held.
func (fc *FailoverController) scheduleProbeLocked() {
	if fc.recoveryTimer != nil {
		fc.recoveryTimer.Stop()
		fc.recoveryTimer = nil
	}

	delay := fc.currentDelay
	gen := fc.generation

	fc.nextProbeAt = fc.scheduler.Now().Add(delay)
	fc.recoveryTimer = fc.scheduler.AfterFunc(delay, func() {
		fc.runProbe(gen)
	})
}

// runProbe executes a one-shot TCP probe against the current recovery target.
func (fc *FailoverController) runProbe(gen uint64) {
	fc.mu.Lock()
	// Stale probe from an older generation: ignore without mutating state.
	if gen != fc.generation {
		fc.mu.Unlock()
		return
	}
	// Only probe if we're in fallback_active or recovering.
	if fc.state != stateFallbackActive && fc.state != stateRecovering {
		fc.mu.Unlock()
		return
	}
	// At most one probe in flight. If a probe is already running (which should
	// not happen given the single pending timer), bail out.
	if fc.probeInFlight {
		fc.mu.Unlock()
		return
	}

	// Capture the recovery target dialer while holding mu; release the mutex
	// for the network I/O. A nil target dialer (defensive) means we cannot
	// probe and we treat it as a failure so the schedule keeps moving.
	targetIdx := fc.recoveryTarget
	var targetDialer *dialer.Dialer
	if targetIdx >= 0 && targetIdx < len(fc.primaryCandidates) {
		targetDialer = fc.primaryCandidates[targetIdx]
	}
	fc.probeInFlight = true

	// Create a cancellable context for this probe.
	ctx, cancel := context.WithTimeout(context.Background(), dialer.Timeout)
	fc.probeCancel = cancel
	fc.mu.Unlock()

	defer cancel()
	defer func() {
		fc.mu.Lock()
		fc.probeCancel = nil
		fc.probeInFlight = false
		fc.mu.Unlock()
	}()

	// Perform the target-aware TCP health check outside the mutex.
	ok, err := fc.probeTarget(ctx, targetDialer)

	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Re-check generation after the network call: a close/reload happened.
	if gen != fc.generation {
		return
	}

	if errors.Is(err, context.Canceled) {
		// Cancellation during shutdown/reload is NOT a failure: leave
		// counters, cursor, and backoff unchanged. Do not reschedule; the
		// close/reload path owns the next schedule.
		return
	}

	if ok && err == nil {
		fc.onProbeSuccessLocked()
		return
	}
	fc.onProbeFailureLocked()
}

// probeTarget runs one TCP connectivity check against the recovery target
// dialer. It prefers the target-aware probeTargetTCP injection seam (used by
// rotation tests to script per-target results) and otherwise falls back to
// the legacy probeTCP closure bound to the initial primary (which is correct
// for the legacy single-primary case where the recovery target is always the
// initial primary at index 0). Each invocation returns its own result.
func (fc *FailoverController) probeTarget(ctx context.Context, d *dialer.Dialer) (bool, error) {
	if fc.probeTargetTCP != nil {
		return fc.probeTargetTCP(ctx, d)
	}
	if fc.probeTCP != nil {
		return fc.probeTCP(ctx)
	}
	return false, errors.New("no probe function configured")
}

// onProbeSuccessLocked handles a successful recovery probe for the current
// recoveryTarget. Must hold mu.
//
// A success always starts or continues recovery confirmation for the current
// target. The first success records stableSince; confirmation probes run at
// recovery_probe_initial (NOT doubled). Promotion requires both
// recovery_successes consecutive successes AND recovery_stable_time elapsed
// since the first success. The attempt threshold never interrupts a target
// producing successes; only failed probes consume the budget. A success does
// NOT erase earlier failures (failedRecoveryProbes is unchanged).
func (fc *FailoverController) onProbeSuccessLocked() {
	fc.recoverySuccesses++

	if fc.recoverySuccesses == 1 {
		// First success — record stability start time.
		fc.stableSince = fc.scheduler.Now()
		fc.log.WithFields(logrus.Fields{
			"group":     fc.groupName,
			"primary":   dialerName(fc.recoveryTargetDialer()),
			"successes": fc.recoverySuccesses,
		}).Info("failback_start")
	}

	// Confirmation probes run at recovery_probe_initial (NOT doubled).
	fc.currentDelay = fc.config.ProbeInitial

	// Transition to recovering if not already there.
	if fc.state == stateFallbackActive {
		fc.state = stateRecovering
		fc.publishSnapshot()
	}

	// Check if recovery is complete: both consecutive successes and stable
	// time elapsed since the first success.
	if fc.recoverySuccesses >= fc.config.Successes &&
		fc.scheduler.Now().Sub(fc.stableSince) >= fc.config.StableTime {
		fc.promoteRecoveryTargetLocked()
		return
	}

	// Schedule next confirmation probe at initial interval.
	fc.scheduleProbeLocked()
}

// promoteRecoveryTargetLocked promotes the current recoveryTarget to
// currentPrimary, resets all rotation/episode state, stops the recovery
// schedule, and emits failback_complete. Must hold mu.
func (fc *FailoverController) promoteRecoveryTargetLocked() {
	promoted := fc.recoveryTarget
	fromName := dialerName(fc.fallback)
	toName := dialerName(fc.recoveryTargetDialer())

	successes := fc.recoverySuccesses
	stableFor := fc.scheduler.Now().Sub(fc.stableSince)

	fc.currentPrimary = promoted
	fc.state = statePrimaryActive
	fc.rotationActive = false
	fc.failedRecoveryProbes = 0
	fc.recoverySuccesses = 0
	fc.stableSince = time.Time{}
	fc.currentDelay = fc.config.ProbeInitial

	// Stop the recovery schedule: cancel timer and any in-flight probe.
	if fc.recoveryTimer != nil {
		fc.recoveryTimer.Stop()
		fc.recoveryTimer = nil
	}
	if fc.probeCancel != nil {
		fc.probeCancel()
		fc.probeCancel = nil
	}

	fc.publishSnapshot()

	fc.log.WithFields(logrus.Fields{
		"group":      fc.groupName,
		"from":       "fallback",
		"to":         "primary",
		"successes":  successes,
		"stable_for": stableFor.Round(time.Second),
	}).Info("failback_complete")

	fc.emitEventLocked(FailoverEvent{
		Type:         FailoverEventFailbackComplete,
		Group:        fc.groupName,
		From:         "fallback",
		To:           "primary",
		Primary:      toName,
		Fallback:     fromName,
		Successes:    successes,
		StableFor:    stableFor,
		TransitionAt: fc.scheduler.Now(),
	})
}

// onProbeFailureLocked handles a failed recovery probe for the current
// recoveryTarget. Must hold mu.
//
// A failed probe (ok=false or a non-cancellation error) consumes one attempt
// from the episode budget: it increments failedRecoveryProbes, clears the
// current target's successes and stableSince, doubles the backoff (capped at
// ProbeMax), and — if rotation is already active — advances recoveryTarget to
// the next candidate circularly. If rotation is not yet active and the
// configured RotationAttempts threshold has been reached, rotation activates
// and recoveryTarget advances to the next candidate after currentPrimary. A
// success never erased earlier failures, so reaching the threshold necessarily
// follows a failed probe.
func (fc *FailoverController) onProbeFailureLocked() {
	fc.failedRecoveryProbes++
	fc.recoverySuccesses = 0
	fc.stableSince = time.Time{}

	// Exponential backoff: double the delay, capped at ProbeMax. The backoff
	// continues across failed targets and stays capped.
	fc.currentDelay = minDuration(fc.currentDelay*2, fc.config.ProbeMax)

	// Advance the recovery target. Rotation is only entered when
	// RotationAttempts > 0; with RotationAttempts == 0 (legacy two-dialer
	// contract) the target never advances and the failed primary keeps being
	// probed with the capped backoff.
	if fc.config.RotationAttempts > 0 {
		if fc.rotationActive {
			fc.recoveryTarget = (fc.recoveryTarget + 1) % len(fc.primaryCandidates)
		} else if fc.failedRecoveryProbes >= fc.config.RotationAttempts {
			fc.rotationActive = true
			fc.recoveryTarget = (fc.currentPrimary + 1) % len(fc.primaryCandidates)
		}
	}

	// Return to fallback_active (snapshot unchanged: still on fallback).
	fc.state = stateFallbackActive
	fc.publishSnapshot()

	if fc.log.IsLevelEnabled(logrus.DebugLevel) {
		fc.log.WithFields(logrus.Fields{
			"group":           fc.groupName,
			"primary":         dialerName(fc.recoveryTargetDialer()),
			"failed_attempts": fc.failedRecoveryProbes,
			"rotation_active": fc.rotationActive,
			"next_in":         fc.currentDelay,
		}).Debug("recovery probe failed, backing off")
	}

	// Schedule next probe (against the possibly-advanced target).
	fc.scheduleProbeLocked()
}

// recoveryTargetDialer returns the dialer of the current recoveryTarget. Must
// be called with mu held. Returns nil if the index is out of range.
func (fc *FailoverController) recoveryTargetDialer() *dialer.Dialer {
	if fc.recoveryTarget < 0 || fc.recoveryTarget >= len(fc.primaryCandidates) {
		return nil
	}
	return fc.primaryCandidates[fc.recoveryTarget]
}

// publishSnapshot updates the atomic snapshot for lock-free reads. Must be
// called with mu held. The snapshot stores the active dialer pointer directly
// so the hot path performs one atomic load and no candidate traversal.
func (fc *FailoverController) publishSnapshot() {
	usingFallback := fc.state == stateFallbackActive || fc.state == stateRecovering
	var active *dialer.Dialer
	if usingFallback {
		active = fc.fallback
	} else {
		active = fc.primaryDialer()
	}
	fc.snapshot.Store(&failoverSnapshot{
		state:         fc.state,
		activeDialer:  active,
		usingFallback: usingFallback,
	})
}

// emitEventLocked constructs and dispatches a FailoverEvent. Must be called
// with mu held; the callback contract requires non-blocking dispatch. If the
// callback is nil or the controller is closed, this is a no-op.
func (fc *FailoverController) emitEventLocked(ev FailoverEvent) {
	if fc.closed || fc.eventCallback == nil {
		return
	}
	fc.eventCallback.OnFailoverEvent(ev)
}

// Close cancels all pending timers and probes. Must be called when the group
// is torn down.
func (fc *FailoverController) Close() {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.closed = true
	fc.generation++
	if fc.recoveryTimer != nil {
		fc.recoveryTimer.Stop()
		fc.recoveryTimer = nil
	}
	if fc.probeCancel != nil {
		fc.probeCancel()
		fc.probeCancel = nil
	}
}

// FailoverControllerSnapshot captures the failover state for warm reload
// inheritance.
type FailoverControllerSnapshot struct {
	State             failoverState
	RecoverySuccesses int
	StableSince       time.Time
	CurrentDelay      time.Duration
	NextProbeAt       time.Time // when the next probe was scheduled to fire
}

// CaptureSnapshot returns a snapshot of the controller state for reload.
func (fc *FailoverController) CaptureSnapshot() FailoverControllerSnapshot {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return FailoverControllerSnapshot{
		State:             fc.state,
		RecoverySuccesses: fc.recoverySuccesses,
		StableSince:       fc.stableSince,
		CurrentDelay:      fc.currentDelay,
		NextProbeAt:       fc.nextProbeAt,
	}
}

// RestoreSnapshot restores controller state from a reload snapshot.
func (fc *FailoverController) RestoreSnapshot(snap FailoverControllerSnapshot) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.state = snap.State
	fc.recoverySuccesses = snap.RecoverySuccesses
	fc.stableSince = snap.StableSince
	fc.currentDelay = snap.CurrentDelay
	fc.publishSnapshot()

	// If we were in fallback/recovering, re-arm the recovery probe with the
	// remaining delay from the old generation.
	if fc.state == stateFallbackActive || fc.state == stateRecovering {
		remaining := time.Until(snap.NextProbeAt)
		if remaining <= 0 || snap.NextProbeAt.IsZero() {
			// nextProbeAt is in the past or unset — fire immediately.
			remaining = time.Millisecond
		}
		originalDelay := fc.currentDelay
		fc.currentDelay = remaining
		fc.scheduleProbeLocked()
		// Restore the original delay for subsequent probes.
		fc.currentDelay = originalDelay
	}
}

// dialerName returns the name of a dialer for logging.
func dialerName(d *dialer.Dialer) string {
	if d == nil {
		return "<nil>"
	}
	if p := d.Property(); p != nil {
		return p.Name
	}
	return "<unknown>"
}
