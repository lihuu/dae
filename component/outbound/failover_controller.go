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

// failoverProbeRun is an identity token for one physical recovery probe. A
// cancelled probe may take time to return, so activeProbe points only to the
// latest probe that is still allowed to mutate controller state. Late results
// and completion defers compare pointer identity before touching shared probe
// ownership.
type failoverProbeRun struct {
	_ byte // non-zero size guarantees distinct addresses for overlapping runs
}

// FailoverRecoveryConfig holds the recovery probe parameters.
type FailoverRecoveryConfig struct {
	Backoff          FailoverProbeBackoff
	ProbeInitial     time.Duration
	ProbeMax         time.Duration
	Successes        int
	StableTime       time.Duration
	RotationAttempts int
}

// FailoverController manages the failover state machine for a DialerGroup.
// It observes each configured Primary's TCP health transitions and drives
// recovery probing with exponential backoff.
type FailoverController struct {
	log       *logrus.Logger
	groupName string

	// primaryCandidates is the ordered list of exact Primary names from the
	// configuration. Index 0 is the initial current Primary.
	primaryCandidates []*dialer.Dialer
	// currentPrimary is the index into primaryCandidates of the dialer that
	// serves new traffic during normal operation. Initialized to 0 on a fresh
	// controller. A confirmed recovery promotion advances this index.
	currentPrimary int
	// recoveryTarget is the index into primaryCandidates of the dialer the
	// recovery probe is currently targeting. Before rotation activates it is
	// the failed current primary; after rotation it advances through the
	// candidates. Initialized to 0 on a fresh controller.
	recoveryTarget int
	fallback       *dialer.Dialer
	config         FailoverRecoveryConfig
	// probeTCP is a LEGACY single-primary TEST injection seam. It is nil in
	// production. When a legacy single-primary test sets it, probeTarget uses it
	// (ignoring the target dialer, which is always the single initial primary in
	// those tests) to drive deterministic probe results. Production MUST NOT set
	// this field: doing so would bind recovery probes to one fixed dialer and
	// silently break rotation, because the production path must probe the dialer
	// at the current recoveryTarget (which advances through the ordered
	// candidates during rotation).
	probeTCP func(context.Context) (bool, error)
	// probeTargetTCP, when set, overrides the production probe for target-aware
	// recovery probes. It receives the dialer of the current recoveryTarget.
	// Rotation tests inject it to script per-target results deterministically.
	// When both probeTargetTCP and probeTCP are nil, probeTarget calls
	// targetDialer.ProbeTCPOnce(ctx) — the production path.
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
	activeProbe       *failoverProbeRun  // logical owner; stale physical probes may still be returning
	generation        uint64             // incremented on close/reload to invalidate stale callbacks

	// rotationActive is true once the current recovery target has exhausted its
	// consecutive-failure budget and recoveryTarget has begun advancing through
	// the ordered Primary candidates.
	rotationActive bool
	// failedRecoveryProbes counts consecutive failed recovery probes for the
	// current recoveryTarget. A successful probe or target advancement resets it.
	failedRecoveryProbes int
	// failedPrimaryName is the name of the primary that triggered the current
	// failover episode. It is recorded when startFailureTransitionLocked runs
	// and is preserved across rotation cursor advances until promotion resets
	// the episode. It supplies the failed_primary field of the
	// primary_rotation_started and recovery_target_advanced structured logs.
	failedPrimaryName string
	// probeInFlight tracks whether the current logical probe owner is running.
	// A cancelled stale physical probe may overlap briefly after reload or
	// rollback, but it no longer owns this flag or any controller state.
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
// candidates are standby Primaries reserved for rotation.
//
// The same controller and state machine handle both one and many candidates.
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

	fc := &FailoverController{
		log:               log,
		groupName:         groupName,
		primaryCandidates: primaryCandidates,
		currentPrimary:    0,
		recoveryTarget:    0,
		fallback:          fallback,
		config:            config,
		// probeTCP is intentionally left nil here: production recovery probes
		// must target the dialer at the current recoveryTarget (which advances
		// through the ordered candidates during rotation), not a closure bound
		// to the initial primary at construction time. probeTarget resolves the
		// production path to targetDialer.ProbeTCPOnce(ctx) when no test seam is
		// injected. probeTCP is a legacy single-primary test injection seam only.
		probeTCP:     nil,
		scheduler:    systemFailoverScheduler{},
		state:        statePrimaryActive,
		currentDelay: config.ProbeInitial,
	}
	fc.publishSnapshot()

	// Register identity-aware alive-transition callbacks for every primary
	// candidate so the controller can distinguish current-primary transitions
	// (which trigger failover) from standby-candidate transitions (which are
	// ignored until that candidate becomes currentPrimary). The closure
	// captures the candidate index so onCandidateHealthChange can decide
	// whether the transition is actionable.
	for i, candidate := range fc.primaryCandidates {
		idx := i
		candidate.RegisterAliveTransitionCallback(func(nt *dialer.NetworkType, alive bool) {
			fc.onCandidateHealthChange(idx, nt, alive)
		})
		candidate.MarkKeepConnectivityCheck()
	}

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

// onPrimaryHealthChange is a legacy test helper that delegates to the
// identity-aware candidate callback for the current primary. It exists so
// legacy tests that call it directly against the single-primary controller
// still compile and pass. Production wiring uses onCandidateHealthChange via
// per-candidate callbacks registered in NewFailoverControllerWithCandidates.
func (fc *FailoverController) onPrimaryHealthChange(networkType *dialer.NetworkType, alive bool) {
	fc.mu.Lock()
	current := fc.currentPrimary
	fc.mu.Unlock()
	fc.onCandidateHealthChange(current, networkType, alive)
}

// onCandidateHealthChange is the identity-aware alive-transition callback for
// every primary candidate. Only a confirmed TCP unavailable transition of the
// currentPrimary triggers failover. UDP transitions and transitions from
// standby candidates are ignored. A controller that is closed or not in the
// healthy-primary state also ignores the transition (a duplicate while in
// fallback/recovering must not re-enter the transition path).
func (fc *FailoverController) onCandidateHealthChange(candidate int, networkType *dialer.NetworkType, alive bool) {
	// Only TCP transitions trigger failover.
	if networkType == nil || networkType.L4Proto != "tcp" {
		return
	}

	if alive {
		// Recovery is driven by the probe loop, not by a single alive
		// transition. Ignore alive transitions here.
		return
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.closed {
		return
	}
	if fc.state != statePrimaryActive {
		// Already in fallback or recovering; ignore duplicate or stale
		// transition. Many concurrent failures collapse into one logical
		// transition.
		return
	}
	if candidate != fc.currentPrimary {
		// A standby candidate's transition does not affect the active
		// selection. Only currentPrimary transitions trigger failover.
		return
	}

	fc.startFailureTransitionLocked("tcp_unavailable")
}

// startFailureTransitionLocked performs the Failure Transition for the current
// primary: selects the fixed fallback for new traffic, sets recoveryTarget to
// the failed current primary, resets rotationActive and failedRecoveryProbes,
// resets the backoff, schedules the first probe, and emits failover_switch.
// Must hold mu. Called from the production candidate health callback and the
// deterministic test seam triggerPrimaryFailureForTest.
func (fc *FailoverController) startFailureTransitionLocked(trigger string) {
	fc.failedPrimaryName = dialerName(fc.primaryDialer())

	fc.log.WithFields(logrus.Fields{
		"group":    fc.groupName,
		"from":     "primary",
		"to":       "fallback",
		"trigger":  trigger,
		"primary":  fc.failedPrimaryName,
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
		Primary:      fc.failedPrimaryName,
		Fallback:     dialerName(fc.fallback),
		Trigger:      trigger,
		TransitionAt: fc.scheduler.Now(),
	})

	// Schedule the first recovery probe.
	fc.scheduleProbeLocked()
}

// triggerPrimaryFailureForTest is a test-only seam that performs the Failure
// Transition inline against the current primary without going through the
// alive-transition callback machinery. It exists so the complete rotation
// state machine can be exercised deterministically. Do NOT call from
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

// cancelActiveProbeLocked cancels the current logical probe owner and detaches
// it immediately. Cancellation is best-effort: the physical probe may return
// later, but its run token is no longer active and therefore cannot clear or
// mutate a replacement probe's state. Must be called with mu held.
func (fc *FailoverController) cancelActiveProbeLocked() {
	if fc.probeCancel != nil {
		fc.probeCancel()
	}
	fc.probeCancel = nil
	fc.probeInFlight = false
	fc.activeProbe = nil
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
	// At most one current logical probe is in flight. A stale physical probe
	// detached by reload/rollback does not hold this ownership and therefore
	// does not block an immediate replacement.
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
	// Create a cancellable context for this probe.
	ctx, cancel := context.WithTimeout(context.Background(), dialer.Timeout)
	run := &failoverProbeRun{}
	fc.activeProbe = run
	fc.probeInFlight = true
	fc.probeCancel = cancel
	fc.mu.Unlock()

	defer cancel()
	defer func() {
		fc.mu.Lock()
		if fc.activeProbe == run {
			fc.activeProbe = nil
			fc.probeCancel = nil
			fc.probeInFlight = false
		}
		fc.mu.Unlock()
	}()

	// Perform the target-aware TCP health check outside the mutex.
	ok, err := fc.probeTarget(ctx, targetDialer)

	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Re-check both generation and run ownership after the network call. A
	// reload/rollback can detach this run and start a replacement before the
	// cancelled physical probe returns.
	if gen != fc.generation || fc.activeProbe != run {
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
// dialer d. Resolution order:
//
//  1. probeTargetTCP (target-aware test seam) — rotation tests inject it to
//     script per-target results deterministically.
//  2. probeTCP (legacy single-primary test seam) — legacy single-primary tests
//     inject it to drive deterministic results; the target dialer is always
//     the single initial primary in those tests, so ignoring d is safe.
//  3. d.ProbeTCPOnce(ctx) — the PRODUCTION path. This must probe the dialer at
//     the current recoveryTarget, which advances through the ordered candidates
//     during rotation. Binding production to a fixed closure would silently
//     break rotation by always probing the initial primary.
//
// A nil target dialer (defensive; callers guard against this) returns a failure
// result so the schedule keeps moving.
func (fc *FailoverController) probeTarget(ctx context.Context, d *dialer.Dialer) (bool, error) {
	if d == nil {
		return false, errors.New("no target dialer to probe")
	}
	if fc.probeTargetTCP != nil {
		return fc.probeTargetTCP(ctx, d)
	}
	if fc.probeTCP != nil {
		return fc.probeTCP(ctx)
	}
	return d.ProbeTCPOnce(ctx)
}

// onProbeSuccessLocked handles a successful recovery probe for the current
// recoveryTarget. Must hold mu.
//
// A success always starts or continues recovery confirmation for the current
// target. The first success records stableSince; confirmation probes run at
// recovery_probe_initial (NOT doubled). Promotion requires both
// recovery_successes consecutive successes AND recovery_stable_time elapsed
// since the first success. The attempt threshold never interrupts a target
// producing successes; only failed probes consume the budget. A success resets
// the current target's consecutive failure count.
func (fc *FailoverController) onProbeSuccessLocked() {
	fc.failedRecoveryProbes = 0
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
	fc.failedPrimaryName = ""
	fc.recoverySuccesses = 0
	fc.stableSince = time.Time{}
	fc.currentDelay = fc.config.ProbeInitial

	// Stop the recovery schedule: cancel timer and any in-flight probe.
	if fc.recoveryTimer != nil {
		fc.recoveryTimer.Stop()
		fc.recoveryTimer = nil
	}
	fc.cancelActiveProbeLocked()

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
// A failed probe (ok=false or a non-cancellation error) increments the current
// target's consecutive failure count, clears confirmation state, and doubles
// the backoff (capped at ProbeMax). When a positive RotationAttempts threshold
// is reached, recoveryTarget advances circularly and the consecutive count is
// reset. The first advance emits primary_rotation_started; later advances emit
// recovery_target_advanced. A success resets the consecutive count without
// promoting the target until recovery confirmation completes.
func (fc *FailoverController) onProbeFailureLocked() {
	fc.failedRecoveryProbes++
	failedAttempts := fc.failedRecoveryProbes
	fc.recoverySuccesses = 0
	fc.stableSince = time.Time{}

	// Capture the cursor edge for the structured rotation logs. fromTarget is
	// the target whose probe just failed; toTarget is the next target the
	// cursor will advance to. For the first threshold transition, fromTarget is
	// the failed current Primary and toTarget is the next configured Primary.
	fromTargetIdx := fc.recoveryTarget
	fromTargetName := dialerName(fc.recoveryTargetDialer())

	advances := fc.config.RotationAttempts > 0 &&
		fc.failedRecoveryProbes >= fc.config.RotationAttempts
	rotationStarts := advances && !fc.rotationActive

	if advances {
		fc.rotationActive = true
		fc.recoveryTarget = (fc.recoveryTarget + 1) % len(fc.primaryCandidates)
		fc.failedRecoveryProbes = 0
	}

	fc.currentDelay = nextRecoveryProbeDelay(fc.currentDelay, fc.config, advances)

	toTargetName := dialerName(fc.recoveryTargetDialer())

	// Return to fallback_active (snapshot unchanged: still on fallback).
	fc.state = stateFallbackActive
	fc.publishSnapshot()

	if fc.log.IsLevelEnabled(logrus.DebugLevel) {
		if advances {
			eventName := "recovery_target_advanced"
			if rotationStarts {
				eventName = "primary_rotation_started"
			}
			fc.log.WithFields(logrus.Fields{
				"group":           fc.groupName,
				"failed_primary":  fc.failedPrimaryName,
				"from_target":     fromTargetName,
				"to_target":       toTargetName,
				"failed_attempts": failedAttempts,
				"next_probe_in":   fc.currentDelay,
			}).Debug(eventName)
		} else {
			fc.log.WithFields(logrus.Fields{
				"group":           fc.groupName,
				"primary":         dialerName(fc.recoveryTargetDialer()),
				"failed_attempts": failedAttempts,
				"rotation_active": fc.rotationActive,
				"next_in":         fc.currentDelay,
			}).Debug("recovery probe failed, backing off")
		}
	}
	_ = fromTargetIdx

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

// currentPrimaryDialerLocked returns the dialer of the currentPrimary. Must be
// called with mu held. Returns nil if the index is out of range. This is the
// locked-path alias of primaryDialer used by the dynamic-event helpers.
func (fc *FailoverController) currentPrimaryDialerLocked() *dialer.Dialer {
	if fc.currentPrimary < 0 || fc.currentPrimary >= len(fc.primaryCandidates) {
		return nil
	}
	return fc.primaryCandidates[fc.currentPrimary]
}

// recoveryTargetDialerLocked is a locked-path alias of recoveryTargetDialer
// for the dynamic-event helpers. Must be called with mu held.
func (fc *FailoverController) recoveryTargetDialerLocked() *dialer.Dialer {
	return fc.recoveryTargetDialer()
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
//
// A notifier (Bark dispatch) failure must remain isolated: it cannot change
// failover state, timers, counters, or the selected dialer. The callback is
// invoked with panic recovery so a panicking notifier cannot unwind through
// the locked transition path. The mutex is still released by the caller's
// deferred Unlock because Go defers run during panic unwinding, but the
// recover here prevents the unwinding entirely.
func (fc *FailoverController) emitEventLocked(ev FailoverEvent) {
	if fc.closed || fc.eventCallback == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			fc.log.WithFields(logrus.Fields{
				"group": fc.groupName,
				"event": string(ev.Type),
				"panic": r,
			}).Warn("failover notifier panic recovered; state machine unaffected")
		}
	}()
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
	fc.cancelActiveProbeLocked()
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
