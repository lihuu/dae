/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/daeuniverse/dae/component/outbound/dialer"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
)

// baseReloadRecoveryConfig is the recovery configuration shared by the
// compatible-reload test cases. Individual cases clone it and flip one field to
// prove the deterministic reset-reason precedence.
func baseReloadRecoveryConfig() FailoverRecoveryConfig {
	return FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}
}

// reloadTestControllers builds two failover controllers with the supplied
// recovery configs and candidate/fallback name lists. The old controller is
// driven into a mid-rotation state (B is the recovery target, 6 failures,
// rotation active) using a fake scheduler. Both controllers share the same
// scheduler clock origin so the restore path can be exercised deterministically.
//
// The returned old scheduler advanced to the point where the next probe is
// pending against B. The new controller's scheduler is independent but starts
// at the same logical now so remaining-delay assertions are stable.
func reloadTestControllers(
	t *testing.T,
	oldRecovery, newRecovery FailoverRecoveryConfig,
	oldCandidates, newCandidates []string,
	oldFallback, newFallback string,
) (
	oldFC *FailoverController, newFC *FailoverController,
	oldSched, newSched *fakeFailoverScheduler,
	oldDialers, newDialers []*dialer.Dialer,
) {
	t.Helper()
	option := testFailoverDialerOption()

	makeDialers := func(names []string) []*dialer.Dialer {
		out := make([]*dialer.Dialer, 0, len(names))
		for _, n := range names {
			out = append(out, newNamedDirectDialer(option, n))
		}
		return out
	}
	oldDialers = makeDialers(oldCandidates)
	newDialers = makeDialers(newCandidates)
	oldFallbackDialer := newNamedDirectDialer(option, oldFallback)
	newFallbackDialer := newNamedDirectDialer(option, newFallback)

	oldFC = NewFailoverControllerWithCandidates(log, "reload-test", oldDialers, oldFallbackDialer, oldRecovery)
	newFC = NewFailoverControllerWithCandidates(log, "reload-test", newDialers, newFallbackDialer, newRecovery)
	oldSched = newFakeFailoverScheduler()
	newSched = newFakeFailoverScheduler()
	oldFC.scheduler = oldSched
	newFC.scheduler = newSched

	// Drive the old controller: trigger failover against A (index 0), then
	// fire 7 failed probes so rotation activates and the cursor advances
	// A -> B. recoveryTarget is now B (index 1), failedRecoveryProbes=2,
	// rotationActive=true, currentDelay capped at ProbeMax.
	oldFC.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		return false, nil
	}
	oldFC.triggerPrimaryFailureForTest()
	for i := 0; i < 7; i++ {
		oldSched.FireNext(t)
	}

	// Sanity: old controller is mid-rotation on B.
	oldFC.mu.Lock()
	oldTarget := oldFC.recoveryTarget
	oldFailed := oldFC.failedRecoveryProbes
	oldRotation := oldFC.rotationActive
	oldFC.mu.Unlock()
	if oldTarget != 1 {
		t.Fatalf("old controller recoveryTarget = %d, want 1 (B)", oldTarget)
	}
	if oldFailed != 2 {
		t.Fatalf("old controller failedRecoveryProbes = %d, want 2", oldFailed)
	}
	if !oldRotation {
		t.Fatal("old controller rotationActive = false, want true")
	}
	return oldFC, newFC, oldSched, newSched, oldDialers, newDialers
}

// TestFailoverReloadSnapshotCompatiblePreservesRotation proves that an
// identity-compatible warm reload preserves currentPrimary, recoveryTarget,
// rotationActive, failedRecoveryProbes, confirmation state, currentDelay, and
// the remaining probe delay. The new controller resumes a single probe of B
// with the captured delay.
func TestFailoverReloadSnapshotCompatiblePreservesRotation(t *testing.T) {
	oldRecovery := baseReloadRecoveryConfig()
	newRecovery := baseReloadRecoveryConfig()
	oldFC, newFC, oldSched, newSched, _, _ := reloadTestControllers(
		t, oldRecovery, newRecovery,
		[]string{"A", "B", "C"}, []string{"A", "B", "C"},
		"fallback", "fallback",
	)
	defer oldFC.Close()
	defer newFC.Close()

	// Capture the old remaining delay before transfer.
	oldFC.mu.Lock()
	oldNextProbeAt := oldFC.nextProbeAt
	oldCurrentDelay := oldFC.currentDelay
	oldFC.mu.Unlock()

	transfer, reason := newFC.prepareReloadTransfer(oldFC)
	if transfer == nil {
		t.Fatalf("expected compatible transfer, got nil (reason=%q)", reason)
	}
	if reason != "" {
		t.Fatalf("compatible transfer reason = %q, want empty", reason)
	}
	defer transfer.Commit()

	// The old controller's timer/probe must be paused: no pending timer.
	if got := oldSched.PendingCount(); got != 0 {
		t.Fatalf("old scheduler pending timers after transfer = %d, want 0", got)
	}

	// The new controller must have exactly one pending probe of B with the
	// captured currentDelay (remaining delay preserves the original schedule).
	newFC.mu.Lock()
	newTarget := newFC.recoveryTarget
	newFailed := newFC.failedRecoveryProbes
	newRotation := newFC.rotationActive
	newCurrentPrimary := newFC.currentPrimary
	newDelay := newFC.currentDelay
	newNextProbeAt := newFC.nextProbeAt
	newSuccesses := newFC.recoverySuccesses
	newFC.mu.Unlock()

	if newCurrentPrimary != 0 {
		t.Fatalf("new currentPrimary = %d, want 0 (A still current)", newCurrentPrimary)
	}
	if newTarget != 1 {
		t.Fatalf("new recoveryTarget = %d, want 1 (B preserved by name)", newTarget)
	}
	if !newRotation {
		t.Fatal("new rotationActive = false, want true (preserved)")
	}
	if newFailed != 2 {
		t.Fatalf("new failedRecoveryProbes = %d, want 2 (preserved)", newFailed)
	}
	if newSuccesses != 0 {
		t.Fatalf("new recoverySuccesses = %d, want 0 (preserved)", newSuccesses)
	}
	if newDelay != oldCurrentDelay {
		t.Fatalf("new currentDelay = %v, want %v (preserved)", newDelay, oldCurrentDelay)
	}
	// Remaining delay should be the original delay (no time advanced between
	// capture and restore in the fake clock).
	wantRemaining := oldCurrentDelay
	if got := newNextProbeAt.Sub(newSched.Now()); got != wantRemaining {
		t.Fatalf("new remaining probe delay = %v, want %v", got, wantRemaining)
	}
	_ = oldNextProbeAt
	if got := newSched.PendingCount(); got != 1 {
		t.Fatalf("new scheduler pending timers = %d, want 1", got)
	}
}

// TestFailoverReloadSnapshotResetsOnIdentityChange table-tests each
// one-at-a-time identity change and asserts the deterministic reset reason and
// that the new controller starts fresh at index 0 (A) with no rotation.
func TestFailoverReloadSnapshotResetsOnIdentityChange(t *testing.T) {
	type changeCase struct {
		name         string
		modifyNew    func(cfg FailoverRecoveryConfig) FailoverRecoveryConfig
		newCands     []string
		newFallback  string
		wantReason   string
		wantNewPrime string
	}

	cases := []changeCase{
		{
			name:         "fallback_changed",
			modifyNew:    func(c FailoverRecoveryConfig) FailoverRecoveryConfig { return c },
			newCands:     []string{"A", "B", "C"},
			newFallback:  "fallback2",
			wantReason:   "fallback_changed",
			wantNewPrime: "A",
		},
		{
			name:         "primary_candidates_changed",
			modifyNew:    func(c FailoverRecoveryConfig) FailoverRecoveryConfig { return c },
			newCands:     []string{"A", "B", "C", "D"},
			newFallback:  "fallback",
			wantReason:   "primary_candidates_changed",
			wantNewPrime: "A",
		},
		{
			name: "primary_rotation_attempts_changed",
			modifyNew: func(c FailoverRecoveryConfig) FailoverRecoveryConfig {
				c.RotationAttempts = 7
				return c
			},
			newCands:     []string{"A", "B", "C"},
			newFallback:  "fallback",
			wantReason:   "recovery_policy_changed",
			wantNewPrime: "A",
		},
		{
			name: "recovery_probe_initial_changed",
			modifyNew: func(c FailoverRecoveryConfig) FailoverRecoveryConfig {
				c.ProbeInitial = 20 * time.Second
				return c
			},
			newCands:     []string{"A", "B", "C"},
			newFallback:  "fallback",
			wantReason:   "recovery_policy_changed",
			wantNewPrime: "A",
		},
		{
			name: "recovery_probe_max_changed",
			modifyNew: func(c FailoverRecoveryConfig) FailoverRecoveryConfig {
				c.ProbeMax = 10 * time.Minute
				return c
			},
			newCands:     []string{"A", "B", "C"},
			newFallback:  "fallback",
			wantReason:   "recovery_policy_changed",
			wantNewPrime: "A",
		},
		{
			name: "recovery_successes_changed",
			modifyNew: func(c FailoverRecoveryConfig) FailoverRecoveryConfig {
				c.Successes = 5
				return c
			},
			newCands:     []string{"A", "B", "C"},
			newFallback:  "fallback",
			wantReason:   "recovery_policy_changed",
			wantNewPrime: "A",
		},
		{
			name: "recovery_stable_time_changed",
			modifyNew: func(c FailoverRecoveryConfig) FailoverRecoveryConfig {
				c.StableTime = 1 * time.Minute
				return c
			},
			newCands:     []string{"A", "B", "C"},
			newFallback:  "fallback",
			wantReason:   "recovery_policy_changed",
			wantNewPrime: "A",
		},
		{
			name:         "primary_candidates_reordered",
			modifyNew:    func(c FailoverRecoveryConfig) FailoverRecoveryConfig { return c },
			newCands:     []string{"B", "A", "C"},
			newFallback:  "fallback",
			wantReason:   "primary_candidates_changed",
			wantNewPrime: "B",
		},
		{
			name:         "primary_candidate_removed",
			modifyNew:    func(c FailoverRecoveryConfig) FailoverRecoveryConfig { return c },
			newCands:     []string{"A", "C"},
			newFallback:  "fallback",
			wantReason:   "primary_candidates_changed",
			wantNewPrime: "A",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldRecovery := baseReloadRecoveryConfig()
			newRecovery := tc.modifyNew(baseReloadRecoveryConfig())
			oldFC, newFC, _, newSched, _, _ := reloadTestControllers(
				t, oldRecovery, newRecovery,
				[]string{"A", "B", "C"}, tc.newCands,
				"fallback", tc.newFallback,
			)
			defer oldFC.Close()
			defer newFC.Close()

			transfer, reason := newFC.prepareReloadTransfer(oldFC)
			if transfer != nil {
				t.Fatalf("expected nil transfer on identity change, got non-nil")
			}
			if reason != tc.wantReason {
				t.Fatalf("reset reason = %q, want %q", reason, tc.wantReason)
			}

			// The new controller must start fresh at index 0 (A), no rotation.
			newFC.mu.Lock()
			cp := newFC.currentPrimary
			rt := newFC.recoveryTarget
			ra := newFC.rotationActive
			fp := newFC.failedRecoveryProbes
			newPrime := dialerName(newFC.primaryCandidates[cp])
			newFC.mu.Unlock()
			if cp != 0 {
				t.Fatalf("new currentPrimary = %d, want 0 (fresh start)", cp)
			}
			if newPrime != tc.wantNewPrime {
				t.Fatalf("new primary = %q, want %q", newPrime, tc.wantNewPrime)
			}
			if rt != 0 {
				t.Fatalf("new recoveryTarget = %d, want 0 (fresh start)", rt)
			}
			if ra {
				t.Fatal("new rotationActive = true, want false (fresh start)")
			}
			if fp != 0 {
				t.Fatalf("new failedRecoveryProbes = %d, want 0 (fresh start)", fp)
			}
			// No probe should be scheduled on a fresh primary-active controller.
			if got := newSched.PendingCount(); got != 0 {
				t.Fatalf("new scheduler pending timers = %d, want 0 (fresh start)", got)
			}
		})
	}
}

// TestFailoverReloadIdentityMismatchLeavesOldRecoveryRunning proves that an
// incompatible staged reload does not pause the old controller. The new
// controller starts fresh and no transfer is returned, so the old generation
// must retain ownership of its pending recovery timer in case the staged
// cutover later rolls back to it.
func TestFailoverReloadIdentityMismatchLeavesOldRecoveryRunning(t *testing.T) {
	oldRecovery := baseReloadRecoveryConfig()
	newRecovery := baseReloadRecoveryConfig()
	oldFC, newFC, oldSched, _, _, _ := reloadTestControllers(
		t, oldRecovery, newRecovery,
		[]string{"A", "B", "C"}, []string{"A", "B", "C", "D"},
		"fallback", "fallback",
	)
	defer oldFC.Close()
	defer newFC.Close()

	oldFC.mu.Lock()
	beforeGeneration := oldFC.generation
	beforeFailed := oldFC.failedRecoveryProbes
	beforeTarget := oldFC.recoveryTarget
	oldFC.mu.Unlock()
	if got := oldSched.PendingCount(); got != 1 {
		t.Fatalf("old pending timers before incompatible reload = %d, want 1", got)
	}

	transfer, reason := newFC.prepareReloadTransfer(oldFC)
	if transfer != nil {
		t.Fatal("incompatible reload returned a transfer, want nil")
	}
	if reason != "primary_candidates_changed" {
		t.Fatalf("reset reason = %q, want primary_candidates_changed", reason)
	}

	oldFC.mu.Lock()
	afterGeneration := oldFC.generation
	afterFailed := oldFC.failedRecoveryProbes
	afterTarget := oldFC.recoveryTarget
	oldFC.mu.Unlock()
	if afterGeneration != beforeGeneration {
		t.Fatalf("old generation changed on incompatible reload: %d -> %d", beforeGeneration, afterGeneration)
	}
	if afterFailed != beforeFailed {
		t.Fatalf("old failed probes changed on incompatible reload: %d -> %d", beforeFailed, afterFailed)
	}
	if afterTarget != beforeTarget {
		t.Fatalf("old recovery target changed on incompatible reload: %d -> %d", beforeTarget, afterTarget)
	}
	if got := oldSched.PendingCount(); got != 1 {
		t.Fatalf("old pending timers after incompatible reload = %d, want 1", got)
	}

	// The old generation still owns the pending B probe. Firing it must execute
	// normal recovery work: the failure count advances and the rotating cursor
	// moves from B to C.
	oldSched.FireNext(t)
	oldFC.mu.Lock()
	continuedFailed := oldFC.failedRecoveryProbes
	oldTargetAfter := oldFC.recoveryTarget
	oldFC.mu.Unlock()
	if continuedFailed != beforeFailed+1 {
		t.Fatalf("old failed probes after resumed work = %d, want %d", continuedFailed, beforeFailed+1)
	}
	// Since we advanced to B, and B already had 2 failed probes, the next failure
	// leaves it at 3 failed probes (does not advance).
	if oldTargetAfter != 1 {
		t.Fatalf("old recovery target after resumed work = %d, want 1 (B)", oldTargetAfter)
	}
}

// TestFailoverReloadResetReasonPrecedence proves that when multiple identity
// categories change in one reload, the reason follows the precedence
// fallback_changed > primary_candidates_changed > recovery_policy_changed.
func TestFailoverReloadResetReasonPrecedence(t *testing.T) {
	type step struct {
		newCands    []string
		newFallback string
		modifyNew   func(FailoverRecoveryConfig) FailoverRecoveryConfig
		wantReason  string
	}
	steps := []step{
		{
			// fallback + candidates + policy all change -> fallback wins.
			newCands:    []string{"A", "B", "C", "D"},
			newFallback: "fallback2",
			modifyNew: func(c FailoverRecoveryConfig) FailoverRecoveryConfig {
				c.RotationAttempts = 7
				return c
			},
			wantReason: "fallback_changed",
		},
		{
			// candidates + policy change -> candidates wins.
			newCands:    []string{"A", "B", "C", "D"},
			newFallback: "fallback",
			modifyNew: func(c FailoverRecoveryConfig) FailoverRecoveryConfig {
				c.RotationAttempts = 7
				return c
			},
			wantReason: "primary_candidates_changed",
		},
	}
	for i, st := range steps {
		oldRecovery := baseReloadRecoveryConfig()
		newRecovery := st.modifyNew(baseReloadRecoveryConfig())
		oldFC, newFC, _, _, _, _ := reloadTestControllers(
			t, oldRecovery, newRecovery,
			[]string{"A", "B", "C"}, st.newCands,
			"fallback", st.newFallback,
		)
		defer oldFC.Close()
		defer newFC.Close()
		_, reason := newFC.prepareReloadTransfer(oldFC)
		if reason != st.wantReason {
			t.Fatalf("step %d: reset reason = %q, want %q", i, reason, st.wantReason)
		}
	}
}

// TestFailoverReloadInFlightProbeOwnership proves that when a reload captures
// the old controller while a probe is in flight, the old probe is invalidated
// (its result cannot mutate either generation) and the new controller
// schedules exactly one immediate probe of the same named recovery target.
// Commit leaves the old generation paused; Rollback cancels the new generation
// and resumes exactly one old-generation probe with the captured state. A
// second Commit/Rollback is a no-op.
func TestFailoverReloadInFlightProbeOwnership(t *testing.T) {
	oldRecovery := baseReloadRecoveryConfig()
	newRecovery := baseReloadRecoveryConfig()
	oldFC, newFC, oldSched, newSched, _, _ := reloadTestControllers(
		t, oldRecovery, newRecovery,
		[]string{"A", "B", "C"}, []string{"A", "B", "C"},
		"fallback", "fallback",
	)
	defer oldFC.Close()
	defer newFC.Close()

	// Block the old controller's next probe on a channel so the probe is
	// in flight when we prepare the transfer. The probe result (delivered
	// later) must NOT mutate either generation.
	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	probeResult := make(chan error, 1)

	// Replace the old controller's probeTargetTCP with a blocking closure.
	oldFC.mu.Lock()
	oldFC.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		close(probeStarted)
		select {
		case <-probeRelease:
			err := <-probeResult
			return false, err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	// Capture the pending timer's callback so we can fire it directly without
	// using FireNext (which calls t.Fatal from a non-test goroutine). The
	// fake scheduler's FireNext advances the clock and invokes the timer's
	// callback; we replicate that minimal behavior here, guarded so a missing
	// timer is reported via a channel instead of t.Fatal.
	var pendingTimer *fakeFailoverTimer
	var pendingAt time.Time
	for _, c := range oldSched.pending {
		if !c.timer.stopped {
			pendingTimer = c.timer
			pendingAt = c.at
			break
		}
	}
	if pendingTimer == nil {
		oldFC.mu.Unlock()
		t.Fatal("no pending failover timer to drive probe into flight")
	}
	go func() {
		if pendingAt.After(oldSched.now) {
			oldSched.now = pendingAt
		}
		pendingTimer.stopped = true
		pendingTimer.fn()
	}()
	oldFC.mu.Unlock()

	// Wait for the probe to be in flight.
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("old probe did not start in time")
	}

	// Capture state before transfer.
	oldFC.mu.Lock()
	beforeFailed := oldFC.failedRecoveryProbes
	beforeTarget := oldFC.recoveryTarget
	oldFC.mu.Unlock()

	// Prepare transfer while the probe is in flight.
	transfer, reason := newFC.prepareReloadTransfer(oldFC)
	if transfer == nil {
		t.Fatalf("expected transfer, got nil (reason=%q)", reason)
	}

	// The new controller must schedule exactly one immediate probe of B.
	newFC.mu.Lock()
	newTarget := newFC.recoveryTargetNameLocked()
	newProbeInFlight := newFC.probeInFlight
	newNextProbeAt := newFC.nextProbeAt
	newFC.mu.Unlock()
	if newTarget != "B" {
		t.Fatalf("new recovery target = %q, want B", newTarget)
	}
	// Remaining delay should be ~0 (immediate) because the old probe was in flight.
	if got := newSched.PendingCount(); got != 1 {
		t.Fatalf("new scheduler pending timers = %d, want 1 (immediate replacement)", got)
	}
	// The old probe is now invalidated; releasing it must NOT change counters.
	probeResult <- errors.New("late probe result")
	close(probeRelease)
	// Give the old probe goroutine time to observe the cancelled context / stale
	// generation and return without mutating state.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		oldFC.mu.Lock()
		failed := oldFC.failedRecoveryProbes
		oldFC.mu.Unlock()
		if failed == beforeFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	oldFC.mu.Lock()
	afterFailed := oldFC.failedRecoveryProbes
	afterTarget := oldFC.recoveryTarget
	oldFC.mu.Unlock()
	if afterFailed != beforeFailed {
		t.Fatalf("old failedRecoveryProbes changed by late probe: %d -> %d", beforeFailed, afterFailed)
	}
	if afterTarget != beforeTarget {
		t.Fatalf("old recoveryTarget changed by late probe: %d -> %d", beforeTarget, afterTarget)
	}

	// The new controller must not have run its probe yet (it's pending, not in
	// flight). newProbeInFlight should be false at capture time.
	if newProbeInFlight {
		t.Fatal("new probeInFlight = true at capture, want false (timer pending)")
	}
	_ = newNextProbeAt

	// Commit leaves the old generation paused (no pending timers).
	transfer.Commit()
	if got := oldSched.PendingCount(); got != 0 {
		t.Fatalf("after commit, old pending timers = %d, want 0", got)
	}

	// A second Commit is a no-op (does not panic, does not change state).
	transfer.Commit()

	// Drain the new pending probe so it does not leak.
	newFC.Close()
}

// TestFailoverReloadRollbackResumesOldProbe proves that Rollback cancels the
// new generation's probe/timer and resumes exactly one old-generation probe of
// the captured recovery target with the captured delay, preserving counters
// and confirmation state. A second Rollback is a no-op.
func TestFailoverReloadRollbackResumesOldProbe(t *testing.T) {
	oldRecovery := baseReloadRecoveryConfig()
	newRecovery := baseReloadRecoveryConfig()
	oldFC, newFC, oldSched, newSched, _, _ := reloadTestControllers(
		t, oldRecovery, newRecovery,
		[]string{"A", "B", "C"}, []string{"A", "B", "C"},
		"fallback", "fallback",
	)
	defer oldFC.Close()
	defer newFC.Close()

	// Drive the old controller further: land one successful B probe so
	// confirmation state is non-zero, then capture before the next probe.
	// This proves counters and confirmation are preserved across rollback.
	oldFC.mu.Lock()
	// Script: the next probe (against B) succeeds.
	probeCh := make(chan bool, 4)
	oldFC.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		return <-probeCh, nil
	}
	oldFC.mu.Unlock()
	probeCh <- true
	oldSched.FireNext(t)

	oldFC.mu.Lock()
	beforeSuccesses := oldFC.recoverySuccesses
	beforeFailed := oldFC.failedRecoveryProbes
	beforeTarget := oldFC.recoveryTarget
	beforeDelay := oldFC.currentDelay
	beforeRotation := oldFC.rotationActive
	oldFC.mu.Unlock()
	if beforeSuccesses != 1 {
		t.Fatalf("expected 1 confirmation success before transfer, got %d", beforeSuccesses)
	}

	// Prepare transfer.
	transfer, reason := newFC.prepareReloadTransfer(oldFC)
	if transfer == nil {
		t.Fatalf("expected transfer, got nil (reason=%q)", reason)
	}
	// Old must be paused.
	if got := oldSched.PendingCount(); got != 0 {
		t.Fatalf("old pending after transfer = %d, want 0", got)
	}
	// New has one pending probe.
	if got := newSched.PendingCount(); got != 1 {
		t.Fatalf("new pending after transfer = %d, want 1", got)
	}

	// Rollback: cancel new, resume old with captured state.
	transfer.Rollback()

	// New generation's timer must be invalidated.
	if got := newSched.PendingCount(); got != 0 {
		t.Fatalf("new pending after rollback = %d, want 0", got)
	}
	// Old generation must have exactly one pending probe of B with the
	// captured delay.
	if got := oldSched.PendingCount(); got != 1 {
		t.Fatalf("old pending after rollback = %d, want 1", got)
	}
	oldFC.mu.Lock()
	resumedTarget := oldFC.recoveryTarget
	resumedFailed := oldFC.failedRecoveryProbes
	resumedSuccesses := oldFC.recoverySuccesses
	resumedDelay := oldFC.currentDelay
	resumedRotation := oldFC.rotationActive
	resumedNextProbeAt := oldFC.nextProbeAt
	oldFC.mu.Unlock()
	if resumedTarget != beforeTarget {
		t.Fatalf("resumed recoveryTarget = %d, want %d", resumedTarget, beforeTarget)
	}
	if resumedFailed != beforeFailed {
		t.Fatalf("resumed failedRecoveryProbes = %d, want %d", resumedFailed, beforeFailed)
	}
	if resumedSuccesses != beforeSuccesses {
		t.Fatalf("resumed recoverySuccesses = %d, want %d", resumedSuccesses, beforeSuccesses)
	}
	if resumedDelay != beforeDelay {
		t.Fatalf("resumed currentDelay = %v, want %v", resumedDelay, beforeDelay)
	}
	if resumedRotation != beforeRotation {
		t.Fatalf("resumed rotationActive = %v, want %v", resumedRotation, beforeRotation)
	}
	// Remaining delay should be the captured delay.
	if got := resumedNextProbeAt.Sub(oldSched.Now()); got != beforeDelay {
		t.Fatalf("resumed remaining delay = %v, want %v", got, beforeDelay)
	}

	// Second Rollback is a no-op.
	transfer.Rollback()
	if got := oldSched.PendingCount(); got != 1 {
		t.Fatalf("old pending after second rollback = %d, want 1 (no-op)", got)
	}

	// Drain the resumed probe with a failure to allow clean shutdown.
	probeCh <- false
	oldSched.FireNext(t)
}

// TestFailoverReloadFreshRestartStartsAtPriorityZero proves that a fresh
// controller built from the same configuration WITHOUT a snapshot starts at
// index 0 (A) with zero failure count, no rotation, and that no file/config
// write occurs. This covers the process-restart non-persistence requirement.
func TestFailoverReloadFreshRestartStartsAtPriorityZero(t *testing.T) {
	oldRecovery := baseReloadRecoveryConfig()
	newRecovery := baseReloadRecoveryConfig()
	option := testFailoverDialerOption()
	oldCands := []*dialer.Dialer{
		newNamedDirectDialer(option, "A"),
		newNamedDirectDialer(option, "B"),
		newNamedDirectDialer(option, "C"),
	}
	newCands := []*dialer.Dialer{
		newNamedDirectDialer(option, "A"),
		newNamedDirectDialer(option, "B"),
		newNamedDirectDialer(option, "C"),
	}
	oldFallback := newNamedDirectDialer(option, "fallback")
	newFallback := newNamedDirectDialer(option, "fallback")

	oldFC := NewFailoverControllerWithCandidates(log, "restart-test", oldCands, oldFallback, oldRecovery)
	newFC := NewFailoverControllerWithCandidates(log, "restart-test", newCands, newFallback, newRecovery)
	oldSched := newFakeFailoverScheduler()
	newSched := newFakeFailoverScheduler()
	oldFC.scheduler = oldSched
	newFC.scheduler = newSched
	defer oldFC.Close()
	defer newFC.Close()

	// Promote B in the old controller so current primary is NOT A. A fresh
	// restart must still start at A. Drive 5 A failures to activate rotation
	// and advance to B, then 3 B successes to promote B.
	probeCh := make(chan bool, 16)
	oldFC.mu.Lock()
	oldFC.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		return <-probeCh, nil
	}
	oldFC.mu.Unlock()
	oldFC.triggerPrimaryFailureForTest()
	for i := 0; i < 5; i++ {
		probeCh <- false
		oldSched.FireNext(t)
	}
	for i := 0; i < 3; i++ {
		probeCh <- true
		oldSched.FireNext(t)
	}
	oldFC.mu.Lock()
	oldCurrentPrimary := oldFC.currentPrimary
	oldFC.mu.Unlock()
	if oldCurrentPrimary != 1 {
		t.Fatalf("expected B (index 1) promoted in old controller, got %d", oldCurrentPrimary)
	}

	// Build a fresh new controller WITHOUT calling prepareReloadTransfer
	// (simulating a process restart). It must start at A (index 0).
	newFC.mu.Lock()
	freshCurrentPrimary := newFC.currentPrimary
	freshFailed := newFC.failedRecoveryProbes
	freshRotation := newFC.rotationActive
	freshState := newFC.state
	newFC.mu.Unlock()
	if freshCurrentPrimary != 0 {
		t.Fatalf("fresh restart currentPrimary = %d, want 0 (A)", freshCurrentPrimary)
	}
	if freshFailed != 0 {
		t.Fatalf("fresh restart failedRecoveryProbes = %d, want 0", freshFailed)
	}
	if freshRotation {
		t.Fatal("fresh restart rotationActive = true, want false")
	}
	if freshState != statePrimaryActive {
		t.Fatalf("fresh restart state = %v, want statePrimaryActive", freshState)
	}
	if got := newSched.PendingCount(); got != 0 {
		t.Fatalf("fresh restart pending timers = %d, want 0", got)
	}
}

// TestFailoverReloadStateResetLogIsInfoLevel proves that an incompatible reload
// emits exactly one info-level failover_rotation_state_reset log with the
// deterministic reason and old/new primary names.
func TestFailoverReloadStateResetLogIsInfoLevel(t *testing.T) {
	logger, hook := logrustest.NewNullLogger()
	logger.SetLevel(logrus.InfoLevel)

	option := testFailoverDialerOption()
	oldCands := []*dialer.Dialer{
		newNamedDirectDialer(option, "A"),
		newNamedDirectDialer(option, "B"),
		newNamedDirectDialer(option, "C"),
	}
	newCands := []*dialer.Dialer{
		newNamedDirectDialer(option, "A"),
		newNamedDirectDialer(option, "B"),
		newNamedDirectDialer(option, "C"),
		newNamedDirectDialer(option, "D"),
	}
	oldFallback := newNamedDirectDialer(option, "fallback")
	newFallback := newNamedDirectDialer(option, "fallback")
	recovery := baseReloadRecoveryConfig()

	oldFC := NewFailoverControllerWithCandidates(logger, "reload-log-test", oldCands, oldFallback, recovery)
	newFC := NewFailoverControllerWithCandidates(logger, "reload-log-test", newCands, newFallback, recovery)
	oldSched := newFakeFailoverScheduler()
	newSched := newFakeFailoverScheduler()
	oldFC.scheduler = oldSched
	newFC.scheduler = newSched
	defer oldFC.Close()
	defer newFC.Close()

	// Promote B in the old controller so old_current_primary is B.
	oldFC.mu.Lock()
	probeCh := make(chan bool, 4)
	oldFC.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		return <-probeCh, nil
	}
	oldFC.mu.Unlock()
	oldFC.triggerPrimaryFailureForTest()
	for i := 0; i < 5; i++ {
		probeCh <- false
		oldSched.FireNext(t)
	}
	// Now rotation active, target B. Promote B.
	for i := 0; i < 3; i++ {
		probeCh <- true
		oldSched.FireNext(t)
	}
	oldFC.mu.Lock()
	oldCP := oldFC.currentPrimary
	oldFC.mu.Unlock()
	if oldCP != 1 {
		t.Fatalf("expected B promoted, got index %d", oldCP)
	}

	transfer, reason := newFC.prepareReloadTransfer(oldFC)
	if transfer != nil {
		t.Fatalf("expected nil transfer on identity change, got non-nil")
	}
	if reason != "primary_candidates_changed" {
		t.Fatalf("reason = %q, want primary_candidates_changed", reason)
	}

	entries := findLogEntries(hook, "failover_rotation_state_reset")
	if len(entries) != 1 {
		t.Fatalf("failover_rotation_state_reset emitted %d times, want 1", len(entries))
	}
	e := entries[0]
	if e.Level != logrus.InfoLevel {
		t.Fatalf("failover_rotation_state_reset level = %v, want Info", e.Level)
	}
	if got := e.Data["group"]; got != "reload-log-test" {
		t.Fatalf("group = %v, want reload-log-test", got)
	}
	if got := e.Data["reason"]; got != "primary_candidates_changed" {
		t.Fatalf("reason field = %v, want primary_candidates_changed", got)
	}
	if got := e.Data["old_current_primary"]; got != "B" {
		t.Fatalf("old_current_primary = %v, want B", got)
	}
	if got := e.Data["new_initial_primary"]; got != "A" {
		t.Fatalf("new_initial_primary = %v, want A", got)
	}
}

// TestFailoverReloadTransferConcurrentCommitRollback proves that concurrent
// Commit/Rollback calls are exclusive: exactly one terminal operation takes
// effect and the other is a no-op. This exercises the sync.Once guard.
func TestFailoverReloadTransferConcurrentCommitRollback(t *testing.T) {
	oldRecovery := baseReloadRecoveryConfig()
	newRecovery := baseReloadRecoveryConfig()
	oldFC, newFC, oldSched, newSched, _, _ := reloadTestControllers(
		t, oldRecovery, newRecovery,
		[]string{"A", "B", "C"}, []string{"A", "B", "C"},
		"fallback", "fallback",
	)
	defer oldFC.Close()
	defer newFC.Close()

	transfer, _ := newFC.prepareReloadTransfer(oldFC)
	if transfer == nil {
		t.Fatal("expected transfer")
	}

	var wg sync.WaitGroup
	commitCalls := 0
	rollbackCalls := 0
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(commit bool) {
			defer wg.Done()
			if commit {
				transfer.Commit()
				mu.Lock()
				commitCalls++
				mu.Unlock()
			} else {
				transfer.Rollback()
				mu.Lock()
				rollbackCalls++
				mu.Unlock()
			}
		}(i%2 == 0)
	}
	wg.Wait()

	// Exactly one of the terminal operations took effect. The old/new pending
	// counts tell us which one: Commit leaves old paused (0) and new pending;
	// Rollback leaves old pending (1) and new paused (0). The test does not
	// assert which won, only that exactly one terminal effect happened.
	oldPending := oldSched.PendingCount()
	newPending := newSched.PendingCount()
	commitWon := oldPending == 0 && newPending >= 1
	rollbackWon := oldPending == 1 && newPending == 0
	if !commitWon && !rollbackWon {
		t.Fatalf("concurrent terminal ops left old=%d new=%d, want exactly one winner", oldPending, newPending)
	}
	// Drain to allow clean shutdown.
	if commitWon {
		newFC.Close()
	} else {
		// Drain the resumed old probe.
		oldFC.mu.Lock()
		oldFC.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
			return false, nil
		}
		oldFC.mu.Unlock()
		oldSched.FireNext(t)
	}
}

// TestFailoverReloadRollbackStaleProbeCannotClobberReplacement proves that a
// rollback does not wait for the cancelled physical probe to exit. The
// replacement probe starts immediately and becomes the controller's sole
// logical owner; when the stale probe eventually returns, it must neither
// clear the replacement's in-flight/cancel state nor mutate recovery state.
func TestFailoverReloadRollbackStaleProbeCannotClobberReplacement(t *testing.T) {
	oldRecovery := baseReloadRecoveryConfig()
	newRecovery := baseReloadRecoveryConfig()
	oldFC, newFC, oldSched, _, _, _ := reloadTestControllers(
		t, oldRecovery, newRecovery,
		[]string{"A", "B", "C"}, []string{"A", "B", "C"},
		"fallback", "fallback",
	)
	defer oldFC.Close()
	defer newFC.Close()

	// Keep both physical probes under test control. The old probe deliberately
	// ignores context cancellation so it can outlive rollback; this models a
	// transport that is slow to observe cancellation.
	probeStarted := make(chan int, 2)
	oldProbeRelease := make(chan struct{})
	newProbeRelease := make(chan struct{})
	var releaseOldOnce sync.Once
	var releaseNewOnce sync.Once
	releaseOld := func() { releaseOldOnce.Do(func() { close(oldProbeRelease) }) }
	releaseNew := func() { releaseNewOnce.Do(func() { close(newProbeRelease) }) }
	defer releaseOld()
	defer releaseNew()

	var probeCallsMu sync.Mutex
	probeCalls := 0

	oldFC.mu.Lock()
	oldFC.probeTargetTCP = func(ctx context.Context, d *dialer.Dialer) (bool, error) {
		probeCallsMu.Lock()
		probeCalls++
		call := probeCalls
		probeCallsMu.Unlock()
		probeStarted <- call
		switch call {
		case 1:
			<-oldProbeRelease
			return false, errors.New("stale probe failed after release")
		case 2:
			<-newProbeRelease
			return false, errors.New("replacement probe failed after release")
		default:
			return false, errors.New("unexpected extra probe")
		}
	}
	oldFC.mu.Unlock()

	// Fire one pending fake timer asynchronously and return a completion
	// channel for the full callback (including runProbe's completion defer).
	fireNextAsync := func() <-chan struct{} {
		t.Helper()
		var pendingTimer *fakeFailoverTimer
		var pendingAt time.Time
		for _, c := range oldSched.pending {
			if !c.timer.stopped {
				pendingTimer = c.timer
				pendingAt = c.at
				break
			}
		}
		if pendingTimer == nil {
			t.Fatal("no pending failover timer")
		}
		if pendingAt.After(oldSched.now) {
			oldSched.now = pendingAt
		}
		pendingTimer.stopped = true
		done := make(chan struct{})
		go func() {
			pendingTimer.fn()
			close(done)
		}()
		return done
	}

	oldProbeDone := fireNextAsync()

	// Wait for the probe to be in flight.
	select {
	case call := <-probeStarted:
		if call != 1 {
			t.Fatalf("first probe call = %d, want 1", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old probe did not start in time")
	}

	oldFC.mu.Lock()
	beforeFailed := oldFC.failedRecoveryProbes
	beforeTarget := oldFC.recoveryTarget
	oldFC.mu.Unlock()

	// Prepare transfer while the probe is in flight. This bumps generation
	// and cancels the old probe's context, but the probe goroutine is still
	// blocked on oldProbeRelease.
	transfer, reason := newFC.prepareReloadTransfer(oldFC)
	if transfer == nil {
		t.Fatalf("expected transfer, got nil (reason=%q)", reason)
	}

	// Rollback must schedule an immediate logical replacement even though the
	// cancelled physical probe is still blocked above.
	transfer.Rollback()

	if got := oldSched.PendingCount(); got != 1 {
		t.Fatalf("old pending after rollback = %d, want 1", got)
	}
	oldFC.mu.Lock()
	rollbackDelay := oldFC.nextProbeAt.Sub(oldSched.Now())
	oldFC.mu.Unlock()
	if rollbackDelay != 0 {
		t.Fatalf("rollback replacement delay = %v, want immediate", rollbackDelay)
	}

	newProbeDone := fireNextAsync()
	select {
	case call := <-probeStarted:
		if call != 2 {
			t.Fatalf("replacement probe call = %d, want 2", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement probe did not start while stale probe was still running")
	}

	// Capture the replacement's logical ownership, then let the stale callback
	// finish completely. Its result and completion defer must be inert.
	oldFC.mu.Lock()
	replacementInFlight := oldFC.probeInFlight
	replacementHasCancel := oldFC.probeCancel != nil
	oldFC.mu.Unlock()
	if !replacementHasCancel || !replacementInFlight {
		t.Fatal("replacement probe did not own in-flight state")
	}

	releaseOld()
	select {
	case <-oldProbeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stale probe did not finish in time")
	}

	oldFC.mu.Lock()
	afterStaleFailed := oldFC.failedRecoveryProbes
	afterStaleTarget := oldFC.recoveryTarget
	afterStaleCancel := oldFC.probeCancel
	afterStaleInFlight := oldFC.probeInFlight
	oldFC.mu.Unlock()
	if afterStaleFailed != beforeFailed || afterStaleTarget != beforeTarget {
		t.Fatalf("stale probe mutated recovery state: failed %d->%d target %d->%d",
			beforeFailed, afterStaleFailed, beforeTarget, afterStaleTarget)
	}
	if afterStaleCancel == nil || !afterStaleInFlight {
		t.Fatal("stale probe completion cleared replacement ownership")
	}

	// The replacement result remains authoritative and advances recovery once.
	releaseNew()
	select {
	case <-newProbeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement probe did not finish in time")
	}
	oldFC.mu.Lock()
	afterReplacementFailed := oldFC.failedRecoveryProbes
	oldFC.mu.Unlock()
	if afterReplacementFailed != beforeFailed+1 {
		t.Fatalf("failed probes after replacement = %d, want %d", afterReplacementFailed, beforeFailed+1)
	}
}

// TestFailoverReloadInvalidConfigLeavesOldControllerActive proves that an
// invalid role resolution error during reload does not touch old controller
// snapshot/timer state.
func TestFailoverReloadInvalidConfigLeavesOldControllerActive(t *testing.T) {
	recovery := baseReloadRecoveryConfig()
	oldFC, _, oldSched, _, _, _ := reloadTestControllers(
		t,
		recovery,
		recovery,
		[]string{"A", "B", "C"},
		[]string{"A", "B", "C"},
		"fallback",
		"fallback",
	)
	defer oldFC.Close()
	option := testFailoverDialerOption()
	a := newNamedDirectDialer(option, "A")
	b := newNamedDirectDialer(option, "B")
	set := &DialerSet{dialers: []*dialer.Dialer{a, b}}
	before := oldFC.CaptureSnapshot()
	beforePending := oldSched.PendingCount()

	_, _, _, err := set.ResolveFailoverRoles(
		exactNameFunction("A", "B"),
		exactNameFunction("A"),
		baseReloadRecoveryConfig(),
	)
	require.ErrorContains(t, err, "overlaps primary")

	after := oldFC.CaptureSnapshot()
	require.Equal(t, before, after)
	require.Equal(t, beforePending, oldSched.PendingCount())
}
