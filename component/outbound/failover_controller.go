/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daeuniverse/dae/component/outbound/dialer"
	"github.com/sirupsen/logrus"
)

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

	// mu protects mutable state below. It must NOT be held during network ops.
	mu sync.Mutex

	state             failoverState
	recoverySuccesses int
	stableSince       time.Time // when the first recovery success occurred
	currentDelay      time.Duration
	recoveryTimer     *time.Timer
	nextProbeAt       time.Time          // when the current recovery timer is scheduled to fire
	probeCancel       context.CancelFunc // cancels the in-flight probe
	generation        uint64             // incremented on close/reload to invalidate stale callbacks

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

	fc.log.WithFields(logrus.Fields{
		"group":    fc.groupName,
		"from":     "primary",
		"to":       "fallback",
		"trigger":  "tcp_unavailable",
		"primary":  dialerName(fc.primaryDialer()),
		"fallback": dialerName(fc.fallback),
	}).Info("failover_switch")

	fc.state = stateFallbackActive
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
		Trigger:      "tcp_unavailable",
		TransitionAt: time.Now(),
	})

	// Schedule the first recovery probe.
	fc.scheduleProbeLocked()
}

// scheduleProbeLocked schedules the next recovery probe. Must be called with mu held.
func (fc *FailoverController) scheduleProbeLocked() {
	if fc.recoveryTimer != nil {
		fc.recoveryTimer.Stop()
		fc.recoveryTimer = nil
	}

	delay := fc.currentDelay
	gen := fc.generation

	fc.nextProbeAt = time.Now().Add(delay)
	fc.recoveryTimer = time.AfterFunc(delay, func() {
		fc.runProbe(gen)
	})
}

// runProbe executes a one-shot TCP probe against the primary dialer.
func (fc *FailoverController) runProbe(gen uint64) {
	fc.mu.Lock()
	// Check if this probe is stale (from an old generation).
	if gen != fc.generation {
		fc.mu.Unlock()
		return
	}
	// Only probe if we're in fallback_active or recovering.
	if fc.state != stateFallbackActive && fc.state != stateRecovering {
		fc.mu.Unlock()
		return
	}

	// Create a cancellable context for this probe.
	ctx, cancel := context.WithTimeout(context.Background(), dialer.Timeout)
	fc.probeCancel = cancel
	fc.mu.Unlock()

	defer cancel()

	// Perform the TCP health check on the primary.
	ok := fc.probePrimaryTCP(ctx)

	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Re-check generation after the network call.
	if gen != fc.generation {
		return
	}

	// Clear the cancel func.
	fc.probeCancel = nil

	if ok {
		fc.onProbeSuccessLocked()
	} else {
		fc.onProbeFailureLocked()
	}
}

// probePrimaryTCP performs a one-shot TCP connectivity check on the primary.
// Each invocation returns its own result and does not depend on a canonical
// health-state transition.
func (fc *FailoverController) probePrimaryTCP(ctx context.Context) bool {
	ok, err := fc.probeTCP(ctx)
	if err != nil && fc.log.IsLevelEnabled(logrus.DebugLevel) {
		fc.log.WithError(err).WithFields(logrus.Fields{
			"group":   fc.groupName,
			"primary": dialerName(fc.primaryDialer()),
		}).Debug("recovery TCP probe failed")
	}
	return ok && err == nil
}

// onProbeSuccessLocked handles a successful recovery probe. Must hold mu.
func (fc *FailoverController) onProbeSuccessLocked() {
	fc.recoverySuccesses++

	if fc.recoverySuccesses == 1 {
		// First success — record stability start time.
		fc.stableSince = time.Now()
		fc.log.WithFields(logrus.Fields{
			"group":     fc.groupName,
			"primary":   dialerName(fc.primaryDialer()),
			"successes": fc.recoverySuccesses,
		}).Info("failback_start")
	}

	// Reset failure backoff.
	fc.currentDelay = fc.config.ProbeInitial

	// Transition to recovering if not already there.
	if fc.state == stateFallbackActive {
		fc.state = stateRecovering
	}

	// Check if recovery is complete.
	if fc.recoverySuccesses >= fc.config.Successes &&
		time.Since(fc.stableSince) >= fc.config.StableTime {
		// Failback!
		fc.log.WithFields(logrus.Fields{
			"group":      fc.groupName,
			"from":       "fallback",
			"to":         "primary",
			"successes":  fc.recoverySuccesses,
			"stable_for": time.Since(fc.stableSince).Round(time.Second),
		}).Info("failback_complete")

		// Capture the values that made this failback qualify, before resetting.
		// StableFor is left unrounded so sub-second stable windows (and the
		// pre-reset value itself) are preserved; the log line above rounds for
		// display only.
		successes := fc.recoverySuccesses
		stableFor := time.Since(fc.stableSince)
		fc.state = statePrimaryActive
		fc.recoverySuccesses = 0
		fc.stableSince = time.Time{}
		fc.currentDelay = fc.config.ProbeInitial
		fc.publishSnapshot()
		fc.emitEventLocked(FailoverEvent{
			Type:         FailoverEventFailbackComplete,
			Group:        fc.groupName,
			From:         "fallback",
			To:           "primary",
			Primary:      dialerName(fc.primaryDialer()),
			Fallback:     dialerName(fc.fallback),
			Successes:    successes,
			StableFor:    stableFor,
			TransitionAt: time.Now(),
		})
		return
	}

	// Schedule next confirmation probe at initial interval.
	fc.scheduleProbeLocked()
}

// onProbeFailureLocked handles a failed recovery probe. Must hold mu.
func (fc *FailoverController) onProbeFailureLocked() {
	fc.recoverySuccesses = 0
	fc.stableSince = time.Time{}

	// Exponential backoff: double the delay, capped at max.
	fc.currentDelay = time.Duration(math.Min(
		float64(fc.currentDelay)*2,
		float64(fc.config.ProbeMax),
	))

	// Return to fallback_active.
	fc.state = stateFallbackActive
	fc.publishSnapshot()

	if fc.log.IsLevelEnabled(logrus.DebugLevel) {
		fc.log.WithFields(logrus.Fields{
			"group":   fc.groupName,
			"primary": dialerName(fc.primaryDialer()),
			"next_in": fc.currentDelay,
		}).Debug("recovery probe failed, backing off")
	}

	// Schedule next probe.
	fc.scheduleProbeLocked()
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
