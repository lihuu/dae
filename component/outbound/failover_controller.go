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
type failoverSnapshot struct {
	state     failoverState
	activeIdx int // index of the active dialer in the group
}

// FailoverRecoveryConfig holds the recovery probe parameters.
type FailoverRecoveryConfig struct {
	ProbeInitial time.Duration
	ProbeMax     time.Duration
	Successes    int
	StableTime   time.Duration
}

// FailoverController manages the failover state machine for a DialerGroup.
// It observes the primary dialer's TCP health transitions and drives recovery
// probing with exponential backoff.
type FailoverController struct {
	log       *logrus.Logger
	groupName string

	primary  *dialer.Dialer
	fallback *dialer.Dialer
	config   FailoverRecoveryConfig

	// mu protects mutable state below. It must NOT be held during network ops.
	mu sync.Mutex

	state             failoverState
	recoverySuccesses int
	stableSince       time.Time // when the first recovery success occurred
	currentDelay      time.Duration
	recoveryTimer     *time.Timer
	probeCancel       context.CancelFunc // cancels the in-flight probe
	generation        uint64             // incremented on close/reload to invalidate stale callbacks

	// snapshot is read lock-free on the selection path.
	snapshot atomic.Pointer[failoverSnapshot]
}

// NewFailoverController creates a new failover controller.
func NewFailoverController(
	log *logrus.Logger,
	groupName string,
	primary *dialer.Dialer,
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

	fc := &FailoverController{
		log:          log,
		groupName:    groupName,
		primary:      primary,
		fallback:     fallback,
		config:       config,
		state:        statePrimaryActive,
		currentDelay: config.ProbeInitial,
	}
	fc.publishSnapshot()

	// Register for primary's TCP health transitions.
	primary.RegisterAliveTransitionCallback(fc.onPrimaryHealthChange)

	return fc
}

// ActiveDialerIndex returns the index of the currently active dialer.
// 0 = primary, 1 = fallback. This is the lock-free hot path.
func (fc *FailoverController) ActiveDialerIndex() int {
	return fc.snapshot.Load().activeIdx
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

	if fc.state != statePrimaryActive {
		// Already in fallback or recovering; ignore duplicate.
		return
	}

	fc.log.WithFields(logrus.Fields{
		"group":    fc.groupName,
		"from":     "primary",
		"to":       "fallback",
		"trigger":  "tcp_unavailable",
		"primary":  dialerName(fc.primary),
		"fallback": dialerName(fc.fallback),
	}).Info("failover_switch")

	fc.state = stateFallbackActive
	fc.recoverySuccesses = 0
	fc.stableSince = time.Time{}
	fc.currentDelay = fc.config.ProbeInitial
	fc.publishSnapshot()

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
func (fc *FailoverController) probePrimaryTCP(ctx context.Context) bool {
	// Reuse the primary dialer's existing TCP health check.
	// We call NotifyCheckTcp which signals the aliveBackground loop to do a
	// check. But we need to observe the result. Instead, we directly check
	// if the primary reports alive for TCP after the check.
	//
	// Actually, the simplest approach: just check if the primary's TCP
	// health is currently alive. If it is, the probe succeeds. If not,
	// trigger a check and wait.
	//
	// But the spec says to reuse the existing TCP health-check implementation.
	// The most reliable way is to observe the Alive state after triggering a
	// check.
	//
	// For now, we use a simpler approach: check TCP4 and TCP6 alive state.
	// If either is alive, the probe succeeds.
	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	tcp6 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "6"}

	// Trigger a check and wait briefly for the result.
	fc.primary.NotifyCheckTcp()

	// Wait a short time for the check to complete, polling the alive state.
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			// Timeout — check one last time.
			return fc.primary.MustGetAlive(tcp4) || fc.primary.MustGetAlive(tcp6)
		case <-ticker.C:
			if fc.primary.MustGetAlive(tcp4) || fc.primary.MustGetAlive(tcp6) {
				return true
			}
		}
	}
}

// onProbeSuccessLocked handles a successful recovery probe. Must hold mu.
func (fc *FailoverController) onProbeSuccessLocked() {
	fc.recoverySuccesses++

	if fc.recoverySuccesses == 1 {
		// First success — record stability start time.
		fc.stableSince = time.Now()
		fc.log.WithFields(logrus.Fields{
			"group":     fc.groupName,
			"primary":   dialerName(fc.primary),
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
			"group":     fc.groupName,
			"from":      "fallback",
			"to":        "primary",
			"successes": fc.recoverySuccesses,
			"stable_for": time.Since(fc.stableSince).Round(time.Second),
		}).Info("failback_complete")

		fc.state = statePrimaryActive
		fc.recoverySuccesses = 0
		fc.stableSince = time.Time{}
		fc.currentDelay = fc.config.ProbeInitial
		fc.publishSnapshot()
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
			"primary": dialerName(fc.primary),
			"next_in": fc.currentDelay,
		}).Debug("recovery probe failed, backing off")
	}

	// Schedule next probe.
	fc.scheduleProbeLocked()
}

// publishSnapshot updates the atomic snapshot for lock-free reads.
func (fc *FailoverController) publishSnapshot() {
	activeIdx := 0
	if fc.state == stateFallbackActive || fc.state == stateRecovering {
		activeIdx = 1
	}
	fc.snapshot.Store(&failoverSnapshot{
		state:     fc.state,
		activeIdx: activeIdx,
	})
}

// Close cancels all pending timers and probes. Must be called when the group
// is torn down.
func (fc *FailoverController) Close() {
	fc.mu.Lock()
	defer fc.mu.Unlock()

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

	// If we were in fallback/recovering, re-arm the recovery probe.
	if fc.state == stateFallbackActive || fc.state == stateRecovering {
		fc.scheduleProbeLocked()
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
