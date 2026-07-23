/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// FailoverReloadIdentity is the set of fields that must be identical across
// two controller generations for warm-reload rotation state to be inherited.
// Identity fields are dialer NAMES (not slice indexes or pointers) so the new
// controller can remap candidates by name even when the dialer slice order or
// underlying dialer pointers differ. The Recovery config carries every
// state-interpreting policy field: primary_rotation_attempts and the four
// recovery parameters.
type FailoverReloadIdentity struct {
	PrimaryCandidates []string
	Fallback          string
	Recovery          FailoverRecoveryConfig
}

// FailoverControllerSnapshot captures the failover state for warm reload
// inheritance. It includes the full rotation state plus timer and in-flight
// probe ownership so the replacement controller can resume exactly one probe.
//
// Name fields (CurrentPrimaryName, RecoveryTargetName) carry identity by name
// across generations; the replacement controller remaps them to its own
// candidate indexes by NAME. RemainingDelay is the probe delay remaining at
// capture time (a duration, not an absolute time) so it is portable across
// scheduler clock origins; restore/rollback reschedule via scheduler.Now().
// NextProbeAt is the absolute scheduled time in the capturing scheduler's frame
// and is retained for the test-facing CaptureSnapshot/RestoreSnapshot wrappers.
type FailoverControllerSnapshot struct {
	State                failoverState
	CurrentPrimaryName   string
	RecoveryTargetName   string
	RotationActive       bool
	FailedRecoveryProbes int // consecutive count for RecoveryTargetName
	RecoverySuccesses    int
	StableSince          time.Time
	CurrentDelay         time.Duration
	NextProbeAt          time.Time
	RemainingDelay       time.Duration
	ProbeInFlight        bool
}

// FailoverReloadTransfer is the transactional handle for a warm reload that
// may preserve failover rotation state. It is created by
// FailoverController.prepareReloadTransfer / DialerGroup.PrepareFailoverReloadFrom.
//
// The transfer pauses the old generation (stops its pending timer, increments
// generation, cancels any in-flight probe) and restores the new generation
// atomically. The caller then drives the transaction to a terminal state:
//
//   - Commit()     — keep the new generation; the old generation stays paused
//     until it is closed (it must NOT resume its probe).
//   - Rollback()   — discard the new generation's restore and resume exactly
//     one old-generation probe of the captured recovery target
//     with the captured delay (preserving counters and
//     confirmation state).
//
// Commit and Rollback are mutually exclusive: a sync.Once guard ensures at
// most one terminal effect runs. Calling either twice is a no-op. Cancellation
// during the transfer is NOT interpreted as a failed recovery result.
type FailoverReloadTransfer struct {
	once     sync.Once
	old      *FailoverController
	next     *FailoverController
	snapshot FailoverControllerSnapshot
}

// Commit finalizes the reload by keeping the new generation. The old
// generation remains paused (its timer and probe were invalidated when the
// transfer was prepared); the caller is responsible for closing it. Idempotent.
func (t *FailoverReloadTransfer) Commit() {
	t.once.Do(func() {
		// No state change required: the new generation is already restored and
		// the old generation's timer/probe were invalidated at prepare time. The
		// caller closes the old controller when it retires the old plane.
	})
}

// Rollback discards the new generation's restore and resumes exactly one
// old-generation probe of the captured recovery target with the captured
// delay. Counters, rotation state, and confirmation state are preserved. The
// new generation's timer/probe is invalidated (and is idempotent with Close).
// Idempotent.
func (t *FailoverReloadTransfer) Rollback() {
	t.once.Do(func() {
		if t.next != nil {
			t.next.cancelReloadRestore()
		}
		if t.old != nil {
			t.old.resumeAfterReloadAbort(t.snapshot)
		}
	})
}

// compareFailoverReloadIdentity returns (true, "") when the two identities are
// compatible for warm-reload inheritance. Otherwise it returns (false, reason)
// where reason follows the deterministic precedence:
//
//	fallback_changed > primary_candidates_changed > recovery_policy_changed
//
// The precedence is fixed so a single reload that changes multiple categories
// produces a stable reason regardless of evaluation order.
func equalFailoverRecoveryConfig(old, next FailoverRecoveryConfig) bool {
	if old.Backoff != next.Backoff ||
		old.ProbeInitial != next.ProbeInitial ||
		old.Successes != next.Successes ||
		old.StableTime != next.StableTime ||
		old.RotationAttempts != next.RotationAttempts {
		return false
	}
	if old.Backoff == FailoverProbeBackoffExponential && old.ProbeMax != next.ProbeMax {
		return false
	}
	return true
}

// compareFailoverReloadIdentity returns (true, "") when the two identities are
// compatible for warm-reload inheritance. Otherwise it returns (false, reason)
// where reason follows the deterministic precedence:
//
//	fallback_changed > primary_candidates_changed > recovery_policy_changed
//
// The precedence is fixed so a single reload that changes multiple categories
// produces a stable reason regardless of evaluation order.
func compareFailoverReloadIdentity(old, next FailoverReloadIdentity) (bool, string) {
	if old.Fallback != next.Fallback {
		return false, "fallback_changed"
	}
	if !slices.Equal(old.PrimaryCandidates, next.PrimaryCandidates) {
		return false, "primary_candidates_changed"
	}
	if !equalFailoverRecoveryConfig(old.Recovery, next.Recovery) {
		return false, "recovery_policy_changed"
	}
	return true, ""
}

// failoverIdentityFromController builds the reload identity from a
// controller's candidate names and recovery config. Must be called with mu
// held (only reads candidate names and the immutable config/fallback).
func failoverIdentityFromController(fc *FailoverController) FailoverReloadIdentity {
	candidates := make([]string, 0, len(fc.primaryCandidates))
	for _, d := range fc.primaryCandidates {
		candidates = append(candidates, dialerName(d))
	}
	return FailoverReloadIdentity{
		PrimaryCandidates: candidates,
		Fallback:          dialerName(fc.fallback),
		Recovery:          fc.config,
	}
}

// captureSnapshotLocked returns a snapshot of the controller state for reload.
// Must be called with mu held. RemainingDelay is the probe delay remaining at
// capture time (a portable duration); NextProbeAt is the absolute scheduled
// time in this scheduler's frame (retained for the test-facing wrappers).
func (fc *FailoverController) captureSnapshotLocked() FailoverControllerSnapshot {
	var remaining time.Duration
	if !fc.nextProbeAt.IsZero() {
		remaining = fc.nextProbeAt.Sub(fc.scheduler.Now())
		if remaining < 0 {
			remaining = 0
		}
	}
	return FailoverControllerSnapshot{
		State:                fc.state,
		CurrentPrimaryName:   dialerName(fc.primaryDialer()),
		RecoveryTargetName:   dialerName(fc.recoveryTargetDialer()),
		RotationActive:       fc.rotationActive,
		FailedRecoveryProbes: fc.failedRecoveryProbes,
		RecoverySuccesses:    fc.recoverySuccesses,
		StableSince:          fc.stableSince,
		CurrentDelay:         fc.currentDelay,
		NextProbeAt:          fc.nextProbeAt,
		RemainingDelay:       remaining,
		ProbeInFlight:        fc.probeInFlight,
	}
}

// CaptureSnapshot returns a snapshot of the controller state for reload. It
// is a test-facing wrapper retained for backward compatibility; production
// warm reload goes through prepareReloadTransfer / PrepareFailoverReloadFrom
// so pausing the old generation and restoring the new generation are one
// atomic operation.
func (fc *FailoverController) CaptureSnapshot() FailoverControllerSnapshot {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.captureSnapshotLocked()
}

// RestoreSnapshot restores controller state from a reload snapshot. It is a
// test-facing wrapper. Production warm reload uses prepareReloadTransfer so
// the old generation's timer/probe are invalidated by generation before the
// new controller acts. The restore uses the scheduler's Now() (not time.Until)
// to compute remaining delay so fake schedulers are deterministic.
func (fc *FailoverController) RestoreSnapshot(snap FailoverControllerSnapshot) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.restoreSnapshotLocked(snap)
}

// restoreSnapshotLocked remaps the snapshot's name-based identity into this
// controller's candidate indexes and restores the rotation state. Must be
// called with mu held. The caller is responsible for ensuring identity
// compatibility (same fallback + same ordered candidate names + same recovery
// policy) before calling.
func (fc *FailoverController) restoreSnapshotLocked(snap FailoverControllerSnapshot) {
	// Remap currentPrimary and recoveryTarget by NAME into this controller's
	// candidate list. A name that is not found leaves the index at its current
	// value (defensive — the compatibility check should have already rejected
	// a mismatch, but we must never carry an index across generations).
	if idx := fc.candidateIndexByName(snap.CurrentPrimaryName); idx >= 0 {
		fc.currentPrimary = idx
	}
	if idx := fc.candidateIndexByName(snap.RecoveryTargetName); idx >= 0 {
		fc.recoveryTarget = idx
	}
	fc.state = snap.State
	fc.rotationActive = snap.RotationActive
	fc.failedRecoveryProbes = snap.FailedRecoveryProbes
	fc.recoverySuccesses = snap.RecoverySuccesses
	fc.stableSince = snap.StableSince
	fc.currentDelay = snap.CurrentDelay
	fc.publishSnapshot()

	// If we were in fallback/recovering, re-arm the recovery probe with the
	// remaining delay computed from the scheduler's clock. If a probe was in
	// flight at capture time, schedule exactly one immediate probe of the same
	// named recovery target (preserving counters, confirmation state, and
	// currentDelay). The replacement must not wait on a past nextProbeAt. The
	// old physical probe may still be returning, but it was invalidated by
	// generation and detached from logical ownership at prepare time.
	if fc.state == stateFallbackActive || fc.state == stateRecovering {
		delay := computeResumeDelay(snap, fc.scheduler.Now())
		originalDelay := fc.currentDelay
		fc.currentDelay = delay
		fc.scheduleProbeLocked()
		fc.currentDelay = originalDelay
	}
}

// computeResumeDelay returns the delay to use when resuming a probe after a
// reload capture. A probe that was in flight at capture time schedules an
// immediate replacement probe (delay 0). Otherwise the portable RemainingDelay
// (a duration captured at transfer time) is used; if it is unset, the legacy
// absolute NextProbeAt is interpreted against the supplied now (the
// test-facing RestoreSnapshot wrapper path). The result is never negative.
func computeResumeDelay(snap FailoverControllerSnapshot, now time.Time) time.Duration {
	if snap.ProbeInFlight {
		return 0
	}
	var delay time.Duration
	if snap.RemainingDelay > 0 {
		delay = snap.RemainingDelay
	} else if !snap.NextProbeAt.IsZero() {
		delay = snap.NextProbeAt.Sub(now)
	}
	if delay < 0 {
		delay = 0
	}
	return delay
}

// candidateIndexByName returns the index of the candidate with the given
// name, or -1 if not found. Must be called with mu held.
func (fc *FailoverController) candidateIndexByName(name string) int {
	for i, d := range fc.primaryCandidates {
		if dialerName(d) == name {
			return i
		}
	}
	return -1
}

// recoveryTargetNameLocked returns the name of the current recovery target.
// Must be called with mu held. Used by tests to assert the named target.
func (fc *FailoverController) recoveryTargetNameLocked() string {
	return dialerName(fc.recoveryTargetDialer())
}

// prepareReloadTransfer is the production warm-reload entry point for a single
// failover controller. It compares the old and new controller identities and,
// on compatibility, atomically pauses the old generation and restores the new
// generation. On incompatibility it returns (nil, reason) and the caller
// (DialerGroup.PrepareFailoverReloadFrom / ControlPlane) emits the
// failover_rotation_state_reset info log; the new controller stays at its
// fresh primary[0] initial candidate.
//
// Compatibility is checked before the old controller is paused. An
// incompatible replacement returns without changing the old timer, generation,
// or in-flight probe, because a failed staged cutover may continue using that
// old controller. On compatibility, lock the old controller, capture the
// snapshot (including whether a probe is in flight), stop the pending timer,
// increment generation, cancel the in-flight probe, and leave the atomic active
// selection unchanged. The old probe result (delivered later) is ignored by the
// generation guard in runProbe and never counts as a recovery failure. Restore
// the new controller with the remaining delay, or one immediate scheduled probe
// when ProbeInFlight is true.
func (fc *FailoverController) prepareReloadTransfer(old *FailoverController) (*FailoverReloadTransfer, string) {
	// Read the replacement identity without holding the old controller lock so
	// the two controller mutexes are never held together.
	fc.mu.Lock()
	newIdentity := failoverIdentityFromController(fc)
	newPrimaryName := dialerName(fc.primaryDialer())
	fc.mu.Unlock()

	old.mu.Lock()
	oldIdentity := failoverIdentityFromController(old)
	compatible, reason := compareFailoverReloadIdentity(oldIdentity, newIdentity)
	if !compatible {
		// Do not pause the old generation. A staged reload can still fail after
		// this point, in which case the old control plane must retain ownership
		// of its existing recovery timer or in-flight probe.
		oldPrimaryName := dialerName(old.primaryDialer())
		old.mu.Unlock()

		fc.log.WithFields(logrus.Fields{
			"group":               fc.groupName,
			"reason":              reason,
			"old_current_primary": oldPrimaryName,
			"new_initial_primary": newPrimaryName,
		}).Info("failover_rotation_state_reset")
		return nil, reason
	}

	snapshot := old.captureSnapshotLocked()
	// Pause the old generation: stop its pending timer, bump generation so a
	// late probe result is ignored, and cancel any in-flight probe. Do NOT
	// mark the old controller closed — the old plane still owns it until the
	// reload commits/rolls back and may close it later.
	if old.recoveryTimer != nil {
		old.recoveryTimer.Stop()
		old.recoveryTimer = nil
	}
	old.generation++
	old.cancelActiveProbeLocked()
	old.mu.Unlock()

	// Restore the new generation. The new controller's mutex is acquired
	// separately (after releasing old) so the two controllers are never
	// locked together.
	fc.mu.Lock()
	fc.restoreSnapshotLocked(snapshot)
	fc.mu.Unlock()

	return &FailoverReloadTransfer{
		old:      old,
		next:     fc,
		snapshot: snapshot,
	}, ""
}

// cancelReloadRestore invalidates the replacement generation's timer and probe
// so a rolled-back reload cannot leave a stale probe running. It is idempotent
// with Close: it bumps the generation and clears the timer/probe without
// marking the controller closed (the caller may still close it). Cancellation
// is NOT interpreted as a failed recovery result.
func (fc *FailoverController) cancelReloadRestore() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.generation++
	if fc.recoveryTimer != nil {
		fc.recoveryTimer.Stop()
		fc.recoveryTimer = nil
	}
	fc.cancelActiveProbeLocked()
}

// resumeAfterReloadAbort re-schedules exactly one old-generation probe of the
// captured recovery target with the captured delay, preserving counters,
// rotation state, and confirmation state. It is called by Rollback after the
// new generation's restore has been invalidated. The captured snapshot
// already reflects the old generation's state at prepare time, so we only
// need to re-arm the schedule.
func (fc *FailoverController) resumeAfterReloadAbort(snap FailoverControllerSnapshot) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	// The old generation's state was not mutated by the transfer (only its
	// timer/probe were invalidated). Remap the named target back to this
	// controller's candidate list defensively in case the snapshot's name
	// differs (it should not on a compatible transfer, but we never carry
	// indexes across generations).
	if idx := fc.candidateIndexByName(snap.RecoveryTargetName); idx >= 0 {
		fc.recoveryTarget = idx
	}
	if idx := fc.candidateIndexByName(snap.CurrentPrimaryName); idx >= 0 {
		fc.currentPrimary = idx
	}
	fc.state = snap.State
	fc.rotationActive = snap.RotationActive
	fc.failedRecoveryProbes = snap.FailedRecoveryProbes
	fc.recoverySuccesses = snap.RecoverySuccesses
	fc.stableSince = snap.StableSince
	fc.currentDelay = snap.CurrentDelay
	fc.publishSnapshot()

	if fc.state == stateFallbackActive || fc.state == stateRecovering {
		// Resume exactly one probe with the captured delay. The old
		// generation was bumped at prepare time; bump it again so any probe
		// that the new generation scheduled (and that may still be pending on
		// a fake scheduler) is ignored by this controller. We must NOT carry
		// the new generation's schedule.
		delay := computeResumeDelay(snap, fc.scheduler.Now())
		originalDelay := fc.currentDelay
		fc.currentDelay = delay
		fc.scheduleProbeLocked()
		fc.currentDelay = originalDelay
	}
}
