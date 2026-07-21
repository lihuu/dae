/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"context"
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/outbound/dialer"
)

// rotationTestNodes holds the named dialers used by the rotation integration
// tests. The dialers are stored separately from the DialerGroup's Dialers
// slice because the group stores them in a shuffled order (fallback, C, A, B)
// to verify that role resolution is independent of dialer slice position.
type rotationTestNodes struct {
	A        *dialer.Dialer
	B        *dialer.Dialer
	C        *dialer.Dialer
	Fallback *dialer.Dialer
}

// testFailoverDialerOption returns a GlobalOption suitable for failover/rotation
// integration tests. The check interval is long (1h) so periodic probes do not
// fire during the test; only traffic-driven callbacks can trigger transitions.
func testFailoverDialerOption() *dialer.GlobalOption {
	return &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     time.Hour,
		CheckTolerance:    0,
	}
}

// newNamedDirectDialer creates a direct dialer with the given name so tests
// can identify which candidate was selected via Property().Name.
func newNamedDirectDialer(option *dialer.GlobalOption, name string) *dialer.Dialer {
	d := newDirectDialer(option, false)
	d.Property().Name = name
	return d
}

// newRotationIntegrationGroup builds a rotation-enabled DialerGroup with
// dialers shuffled into the order [fallback, C, A, B] and priorities
// [1, 3, 0, 2]. This verifies that PrimaryCandidateIdxs is sorted by numeric
// priority (A=0, B=2, C=3) and that the fallback (priority 1) is independent
// of slice position.
func newRotationIntegrationGroup(t *testing.T) (*DialerGroup, rotationTestNodes) {
	t.Helper()
	option := testFailoverDialerOption()
	nodes := rotationTestNodes{
		A:        newNamedDirectDialer(option, "A"),
		B:        newNamedDirectDialer(option, "B"),
		C:        newNamedDirectDialer(option, "C"),
		Fallback: newNamedDirectDialer(option, "fallback"),
	}
	dialers := []*dialer.Dialer{nodes.Fallback, nodes.C, nodes.A, nodes.B}
	annotations := []*dialer.Annotation{
		{Priority: 1}, // Fallback
		{Priority: 3}, // C
		{Priority: 0}, // A
		{Priority: 2}, // B
	}
	cfg, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	})
	if err != nil {
		t.Fatalf("ValidateFailoverGroup failed: %v", err)
	}
	group := NewDialerGroup(
		option,
		"rotation-test",
		dialers,
		annotations,
		DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
		func(bool, *dialer.NetworkType, bool) {},
		cfg,
	)
	t.Cleanup(func() { _ = group.Close() })
	return group, nodes
}

// TestFailoverRotationInitialSelectionAndExclusion verifies the atomic hot
// path selects the initial current Primary (A, priority 0) and that excluding
// the active primary falls through to the fixed fallback (not another
// candidate — rotation is a recovery concern, not a hot-path concern).
func TestFailoverRotationInitialSelectionAndExclusion(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)

	d, _, _, err := group.SelectWithExclusionResult(TestNetworkType, false, nil)
	if err != nil || d != nodes.A {
		t.Fatalf("initial selection = %v, %v; want A", d, err)
	}

	d, _, _, err = group.SelectWithExclusionResult(TestNetworkType, false, nodes.A)
	if err != nil || d != nodes.Fallback {
		t.Fatalf("excluded A selection = %v, %v; want fallback", d, err)
	}
}

// TestFailoverRotationStandbyIdle verifies that standby candidates (B, C) do
// not run periodic probes just by virtue of being candidates. The selection
// state must remain without AliveDialerSets and no standby probe hook fires
// during an idle window.
func TestFailoverRotationStandbyIdle(t *testing.T) {
	option := testFailoverDialerOption()
	option.CheckInterval = 20 * time.Millisecond
	nodes := rotationTestNodes{
		A:        newNamedDirectDialer(option, "A"),
		B:        newNamedDirectDialer(option, "B"),
		C:        newNamedDirectDialer(option, "C"),
		Fallback: newNamedDirectDialer(option, "fallback"),
	}
	dialers := []*dialer.Dialer{nodes.Fallback, nodes.C, nodes.A, nodes.B}
	annotations := []*dialer.Annotation{
		{Priority: 1},
		{Priority: 3},
		{Priority: 0},
		{Priority: 2},
	}
	cfg, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial:     time.Hour,
		ProbeMax:         time.Hour,
		Successes:        3,
		StableTime:       time.Second,
		RotationAttempts: 5,
	})
	if err != nil {
		t.Fatalf("ValidateFailoverGroup failed: %v", err)
	}
	group := NewDialerGroup(
		option,
		"rotation-idle",
		dialers,
		annotations,
		DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
		func(bool, *dialer.NetworkType, bool) {},
		cfg,
	)
	t.Cleanup(func() { _ = group.Close() })

	// Wait several check-intervals. No standby probe wiring exists in this
	// packet, so no transitions should occur and no AliveDialerSet should be
	// constructed for selection.
	time.Sleep(100 * time.Millisecond)

	if state := group.currentSelectionState(); state.aliveDialerSets[0] != nil {
		t.Fatalf("failover rotation group must not create AliveDialerSet, got %v", state.aliveDialerSets[0])
	}
	if d, _ := group.failoverController.ActiveDialer(); d != nodes.A {
		t.Fatalf("active dialer after idle window = %v, want A", d)
	}
}

// scriptedProbeResult pairs an expected target dialer with the result its
// probe should return. driveProbeResults installs a probeTargetTCP closure
// that consumes the scripted results in order, asserting the target identity
// on every probe.
type scriptedProbeResult struct {
	target *dialer.Dialer
	ok     bool
	err    error
}

// installRotationTestScheduler swaps the group's failover controller
// scheduler for a deterministic fake so tests can drive the recovery loop
// without real-time sleeps.
func installRotationTestScheduler(group *DialerGroup) *fakeFailoverScheduler {
	scheduler := newFakeFailoverScheduler()
	group.failoverController.scheduler = scheduler
	return scheduler
}

// triggerCandidateHealth dispatches a candidate alive-transition callback as
// if the candidate's TCP health had changed. It is the identity-aware entry
// point added in Packet 4.
func triggerCandidateHealth(group *DialerGroup, candidate int, networkType *dialer.NetworkType, alive bool) {
	group.failoverController.onCandidateHealthChange(candidate, networkType, alive)
}

// driveProbeResults installs a probeTargetTCP closure that consumes the
// scripted results in order and fires one scheduler timer per result. It
// asserts the target identity on every probe.
func driveProbeResults(
	t *testing.T,
	group *DialerGroup,
	scheduler *fakeFailoverScheduler,
	results ...scriptedProbeResult,
) {
	t.Helper()
	next := 0
	group.failoverController.probeTargetTCP = func(_ context.Context, target *dialer.Dialer) (bool, error) {
		if next >= len(results) {
			t.Fatalf("unexpected probe of %s", target.Property().Name)
		}
		result := results[next]
		next++
		if target != result.target {
			t.Fatalf("probe target = %s, want %s", target.Property().Name, result.target.Property().Name)
		}
		return result.ok, result.err
	}
	for range results {
		scheduler.FireNext(t)
	}
	if next != len(results) {
		t.Fatalf("consumed %d probe results, want %d", next, len(results))
	}
}

// selectedDialer returns the dialer the group currently selects for the test
// TCP network type, failing the test if selection errors out.
func selectedDialer(t *testing.T, group *DialerGroup) *dialer.Dialer {
	t.Helper()
	d, _, _, err := group.SelectWithExclusionResult(TestNetworkType, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestFailoverRotationPromotedBIsSticky verifies that after B is promoted to
// current primary, A becoming healthy later does NOT preempt B. B remains the
// current primary until B itself fails. The first-to-third confirmation
// success span is exactly 30 seconds at the 15-second confirmation cadence.
func TestFailoverRotationPromotedBIsSticky(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)
	scheduler := installRotationTestScheduler(group)

	// A's TCP transition to unavailable triggers failover. B is the first
	// standby candidate; five failed A probes activate rotation and advance
	// the cursor to B, then three B successes at 15s cadence promote B.
	triggerCandidateHealth(group, 0, TestNetworkType, false)
	driveProbeResults(t, group, scheduler,
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
	)
	if d := selectedDialer(t, group); d != nodes.B {
		t.Fatalf("selected %v, want promoted B", d)
	}

	// Advance the clock so the three B successes span 30s of stable time.
	// stableSince is recorded on the first B success; the test fake scheduler
	// starts at time.Unix(0,0) and FireNext advances to each timer's
	// scheduled deadline. The 15s confirmation cadence makes the third
	// success land 30s after the first, satisfying recovery_stable_time.
	_ = scheduler

	// A becomes healthy later — must NOT preempt B.
	triggerCandidateHealth(group, 0, TestNetworkType, true)
	if d := selectedDialer(t, group); d != nodes.B {
		t.Fatalf("A preempted B: selected %v", d)
	}
}

// TestFailoverRotationPromotedBFailureStartsAtC verifies that after B is
// promoted, a later B TCP failure immediately selects the fixed fallback,
// gives B five failed recovery attempts, and then scans starting at C
// (circular from currentPrimary+1).
func TestFailoverRotationPromotedBFailureStartsAtC(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)
	scheduler := installRotationTestScheduler(group)

	// Promote B first.
	triggerCandidateHealth(group, 0, TestNetworkType, false)
	driveProbeResults(t, group, scheduler,
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
	)
	if d := selectedDialer(t, group); d != nodes.B {
		t.Fatalf("selected %v, want promoted B", d)
	}

	// Now B fails. The controller selects the fallback and gives B five
	// failed attempts before advancing to C. The scripted probes below prove
	// the exact sequence: five B probes (the threshold window), then the
	// sixth probe targets C (circular from currentPrimary+1).
	triggerCandidateHealth(group, 1, TestNetworkType, false)
	if d := selectedDialer(t, group); d != nodes.Fallback {
		t.Fatalf("after B failure selected %v, want fallback", d)
	}
	driveProbeResults(t, group, scheduler,
		scriptedProbeResult{target: nodes.B},
		scriptedProbeResult{target: nodes.B},
		scriptedProbeResult{target: nodes.B},
		scriptedProbeResult{target: nodes.B},
		scriptedProbeResult{target: nodes.B},
		scriptedProbeResult{target: nodes.C},
	)

	// After the sixth (C) probe fails, the cursor has advanced past C to A
	// (circular wrap). rotationActive is true and failedRecoveryProbes is 6.
	group.failoverController.mu.Lock()
	currentPrimary := group.failoverController.currentPrimary
	recoveryTarget := group.failoverController.recoveryTarget
	rotationActive := group.failoverController.rotationActive
	failedProbes := group.failoverController.failedRecoveryProbes
	group.failoverController.mu.Unlock()

	if currentPrimary != 1 {
		t.Fatalf("currentPrimary = %d, want 1 (B)", currentPrimary)
	}
	if recoveryTarget != 0 {
		t.Fatalf("recoveryTarget = %d, want 0 (A, circular wrap after C)", recoveryTarget)
	}
	if !rotationActive {
		t.Fatal("rotationActive = false, want true after B's five failures")
	}
	if failedProbes != 6 {
		t.Fatalf("failedRecoveryProbes = %d, want 6", failedProbes)
	}
}

// TestFailoverRotationNonCurrentCandidateFailureIgnored verifies that a TCP
// down transition on a non-current candidate (A) does NOT switch the group
// after B has been promoted. Only transitions on currentPrimary trigger
// failover.
func TestFailoverRotationNonCurrentCandidateFailureIgnored(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)
	scheduler := installRotationTestScheduler(group)

	// Promote B.
	triggerCandidateHealth(group, 0, TestNetworkType, false)
	driveProbeResults(t, group, scheduler,
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
	)
	if d := selectedDialer(t, group); d != nodes.B {
		t.Fatalf("selected %v, want promoted B", d)
	}

	// A (candidate 0) reports TCP down — must not affect the group.
	triggerCandidateHealth(group, 0, TestNetworkType, false)
	if d := selectedDialer(t, group); d != nodes.B {
		t.Fatalf("non-current candidate failure switched group: selected %v, want B", d)
	}
	group.failoverController.mu.Lock()
	state := group.failoverController.state
	group.failoverController.mu.Unlock()
	if state != statePrimaryActive {
		t.Fatalf("state = %v, want statePrimaryActive (non-current candidate ignored)", state)
	}
}

// TestFailoverRotationUDPTransitionOnCurrentPrimaryIgnored verifies that a
// UDP transition on the current primary does NOT trigger failover. Only TCP
// transitions of currentPrimary trigger failover.
func TestFailoverRotationUDPTransitionOnCurrentPrimaryIgnored(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)

	// UDP down on the current primary (A) — must NOT switch.
	triggerCandidateHealth(group, 0, TestDnsUdp4NetworkType, false)
	if d := selectedDialer(t, group); d != nodes.A {
		t.Fatalf("UDP transition switched group: selected %v, want A", d)
	}
	group.failoverController.mu.Lock()
	state := group.failoverController.state
	group.failoverController.mu.Unlock()
	if state != statePrimaryActive {
		t.Fatalf("state = %v, want statePrimaryActive (UDP transitions ignored)", state)
	}
}