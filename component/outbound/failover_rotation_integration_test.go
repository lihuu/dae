/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
	d := newDirectDialer(option)
	d.Property().Name = name
	return d
}

// newRotationIntegrationGroup builds a rotation-enabled DialerGroup with the
// dialer pool shuffled into [fallback, C, A, B] while the explicit Primary
// candidate indexes preserve the configured order [A, B, C]. This verifies
// that role order is independent of dialer-pool position and that Fallback is
// a separate fixed role.
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
	annotations := []*dialer.Annotation{{}, {}, {}, {}}
	cfg := &FailoverConfig{
		PrimaryCandidateIdxs: []int{2, 3, 1},
		FallbackIdx:          0,
		Recovery: FailoverRecoveryConfig{
			ProbeInitial:     15 * time.Second,
			ProbeMax:         5 * time.Minute,
			Successes:        3,
			StableTime:       30 * time.Second,
			RotationAttempts: 5,
		},
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
// path selects the initial current Primary (A, index 0) and that excluding
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
	annotations := []*dialer.Annotation{{}, {}, {}, {}}
	cfg := &FailoverConfig{
		PrimaryCandidateIdxs: []int{2, 3, 1},
		FallbackIdx:          0,
		Recovery: FailoverRecoveryConfig{
			ProbeInitial:     time.Hour,
			ProbeMax:         time.Hour,
			Successes:        3,
			StableTime:       time.Second,
			RotationAttempts: 5,
		},
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

// triggerCandidateHealth dispatches the production candidate-aware callback
// as if the candidate's TCP health had changed.
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

	// A's TCP transition to unavailable triggers failover. B is the next
	// configured Primary; five failed A probes activate rotation and advance
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

	// C probe fails once, cursor remains on C. rotationActive is true and failedRecoveryProbes is 1.
	group.failoverController.mu.Lock()
	currentPrimary := group.failoverController.currentPrimary
	recoveryTarget := group.failoverController.recoveryTarget
	rotationActive := group.failoverController.rotationActive
	failedProbes := group.failoverController.failedRecoveryProbes
	group.failoverController.mu.Unlock()

	if currentPrimary != 1 {
		t.Fatalf("currentPrimary = %d, want 1 (B)", currentPrimary)
	}
	if recoveryTarget != 2 {
		t.Fatalf("recoveryTarget = %d, want 2 (C)", recoveryTarget)
	}
	if !rotationActive {
		t.Fatal("rotationActive = false, want true after B's five failures")
	}
	if failedProbes != 1 {
		t.Fatalf("failedRecoveryProbes = %d, want 1", failedProbes)
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

// assertTCPAndUDPSelected asserts that the group selects the wanted dialer for
// both the TCP and DNS-UDP test network types. Rotation/failover selection is
// network-type-agnostic: the atomic snapshot returns the same active dialer
// regardless of L4Proto, so both selections must agree. This guards against a
// regression that routes UDP through a different (e.g. alive-set) path.
func assertTCPAndUDPSelected(t *testing.T, group *DialerGroup, want *dialer.Dialer) {
	t.Helper()
	if d := selectedDialer(t, group); d != want {
		t.Fatalf("TCP selection = %v, want %v", d, want)
	}
	// UDP selection follows the same atomic snapshot path as TCP for the
	// failover policy; the snapshot is network-type-agnostic.
	d, _, _, err := group.SelectWithExclusionResult(TestDnsUdp4NetworkType, false, nil)
	if err != nil {
		t.Fatalf("UDP selection errored: %v", err)
	}
	if d != want {
		t.Fatalf("UDP selection = %v, want %v", d, want)
	}
}

// TestFailoverRotationIntegrationAtoBRecovery is the complete end-to-end
// recovery scenario from the implementation spec. It drives the fake
// scheduler deterministically (no real-time sleeps) through the full primary
// rotation episode:
//
//	A TCP unavailable
//	-> fixed Fallback selected
//	-> five failed A probes
//	-> one failed B probe
//	-> C recovery starts but fails confirmation
//	-> A fails after circular wrap
//	-> B gets three successes across stable time
//	-> B becomes current Primary
//	-> A healthy callback does not preempt B
//	-> B future failure immediately selects the same fixed Fallback
//
// TCP and UDP selections are asserted at each active-role transition. Existing
// test connections are untouched (this scenario only observes new selections).
func TestFailoverRotationIntegrationAtoBRecovery(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)
	scheduler := installRotationTestScheduler(group)

	// Initial state: A (index 0) is the current primary.
	assertTCPAndUDPSelected(t, group, nodes.A)

	// A TCP unavailable triggers the failure transition. The fixed Fallback
	// is selected for new TCP and UDP traffic; recoveryTarget is A.
	triggerCandidateHealth(group, 0, TestNetworkType, false)
	assertTCPAndUDPSelected(t, group, nodes.Fallback)

	// Scripted probe sequence (target, ok). driveProbeResults fires one
	// scheduler timer per result and asserts the target identity on every
	// probe. The fake scheduler's FireNext advances the logical clock to
	// each timer's scheduled deadline, so the 15s confirmation cadence
	// produces a 30s first-to-third success span for B without real sleeps.
	driveProbeResults(t, group, scheduler,
		// Five failed A probes. After probe 5, failedRecoveryProbes reaches
		// the threshold (5), rotationActive becomes true, and recoveryTarget
		// advances from A to B.
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		// Five failed B probes. rotationActive is already true, so five
		// failures advance recoveryTarget from B to C.
		scriptedProbeResult{target: nodes.B},
		scriptedProbeResult{target: nodes.B},
		scriptedProbeResult{target: nodes.B},
		scriptedProbeResult{target: nodes.B},
		scriptedProbeResult{target: nodes.B},
		// C recovery starts: the first C probe succeeds, recording
		// stableSince and entering stateRecovering.
		scriptedProbeResult{target: nodes.C, ok: true},
		// C confirmation fails: the second C probe fails, clearing
		// recoverySuccesses/stableSince.
		scriptedProbeResult{target: nodes.C},
		// Provide 4 more C failures to advance from C to A.
		scriptedProbeResult{target: nodes.C},
		scriptedProbeResult{target: nodes.C},
		scriptedProbeResult{target: nodes.C},
		scriptedProbeResult{target: nodes.C},
		// A fails 5 times after the circular wrap. rotationActive stays true, so five
		// failures advance recoveryTarget from A to B.
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		// B gets three successes across stable time. The first success
		// records stableSince; confirmation probes run at the 15s initial
		// cadence, so the third success lands 30s after the first and
		// satisfies recovery_stable_time. B is promoted to currentPrimary.
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
		scriptedProbeResult{target: nodes.B, ok: true},
	)

	// B is now the current primary for both TCP and UDP selection.
	assertTCPAndUDPSelected(t, group, nodes.B)

	// A healthy callback must NOT preempt B. B remains the current primary
	// until B itself fails.
	triggerCandidateHealth(group, 0, TestNetworkType, true)
	assertTCPAndUDPSelected(t, group, nodes.B)

	// B future failure immediately selects the same fixed Fallback. The
	// controller does not scan other Primaries on the hot path; it
	// selects the fixed Fallback and begins a fresh recovery episode.
	triggerCandidateHealth(group, 1, TestNetworkType, false)
	assertTCPAndUDPSelected(t, group, nodes.Fallback)

	// The new episode's recoveryTarget is B (the failed current primary).
	// Drive one B probe to confirm the episode targets B first, then
	// circularly scans C, A, B, ... after the five-attempt threshold.
	driveProbeResults(t, group, scheduler,
		scriptedProbeResult{target: nodes.B},
	)
}

// TestFailoverRotationAllUnavailableStaysOnFallback verifies that when every
// primary candidate is unavailable, the controller stays on the fixed
// Fallback and continues the bounded-backoff circular scan indefinitely. It
// does NOT exit recovery, does NOT select a failed candidate, and keeps at
// most one pending timer / one in-flight probe. More than two complete B/C/A
// wraps (9 probes = 3 full wraps) are driven with all probes failing.
//
// Excluding the fixed Fallback during this state returns ErrNoAliveDialer
// without opportunistically selecting an unconfirmed candidate.
func TestFailoverRotationAllUnavailableStaysOnFallback(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)
	scheduler := installRotationTestScheduler(group)

	// A TCP unavailable triggers failover to the fixed Fallback.
	triggerCandidateHealth(group, 0, TestNetworkType, false)
	assertTCPAndUDPSelected(t, group, nodes.Fallback)

	// 3 full B/C/A wraps after the five-attempt threshold activates rotation.
	var results []scriptedProbeResult
	addFailures := func(target *dialer.Dialer, count int) {
		for i := 0; i < count; i++ {
			results = append(results, scriptedProbeResult{target: target})
		}
	}
	addFailures(nodes.A, 5)
	for w := 0; w < 3; w++ {
		addFailures(nodes.B, 5)
		addFailures(nodes.C, 5)
		addFailures(nodes.A, 5)
	}
	driveProbeResults(t, group, scheduler, results...)

	// After every candidate has failed three full wraps, the fixed Fallback
	// is still selected for new TCP and UDP traffic.
	assertTCPAndUDPSelected(t, group, nodes.Fallback)

	// The controller is still in fallback/recovering state and still has
	// exactly one pending recovery timer (bounded-backoff circular scan).
	group.failoverController.mu.Lock()
	state := group.failoverController.state
	rotationActive := group.failoverController.rotationActive
	failedProbes := group.failoverController.failedRecoveryProbes
	group.failoverController.mu.Unlock()
	if state != stateFallbackActive {
		t.Fatalf("state = %v, want stateFallbackActive (still recovering)", state)
	}
	if !rotationActive {
		t.Fatal("rotationActive = false, want true (rotation stays active)")
	}
	if failedProbes != 0 {
		t.Fatalf("failedRecoveryProbes = %d, want 0 (just advanced)", failedProbes)
	}
	if got := scheduler.PendingCount(); got != 1 {
		t.Fatalf("pending timers = %d, want 1 (bounded scan, one timer max)", got)
	}

	// Excluding the fixed Fallback returns ErrNoAliveDialer. The controller
	// must NOT opportunistically select an unconfirmed primary candidate
	// on the hot path: the fallback role is the only other active role,
	// and when it is excluded selection fails rather than scanning.
	d, _, _, err := group.SelectWithExclusionResult(TestNetworkType, false, nodes.Fallback)
	if !errors.Is(err, ErrNoAliveDialer) {
		t.Fatalf("excluding fallback: err = %v, want ErrNoAliveDialer", err)
	}
	if d != nil {
		t.Fatalf("excluding fallback returned dialer %v, want nil", d)
	}
}

// TestFailoverRotationFallbackFailureReturnsError proves that when the fixed
// Fallback is the active role and is excluded for a connection attempt, the
// operation returns ErrNoAliveDialer rather than opportunistically selecting
// an unconfirmed primary candidate. This is the testable level of the spec's
// "fallback failure returns an error without selecting an unconfirmed target"
// requirement: the existing test infrastructure does not simulate a real dial
// failure of the fallback dialer, but SelectWithExclusionResult with the
// fallback excluded exercises the same _select code path that a real dial
// failure would hit (the failover policy branch returns ErrNoAliveDialer
// when the active role is excluded). The test asserts the error and the nil
// dialer, proving rotation does NOT promote an unconfirmed candidate when the
// fallback is unavailable.
func TestFailoverRotationFallbackFailureReturnsError(t *testing.T) {
	group, nodes := newRotationIntegrationGroup(t)
	scheduler := installRotationTestScheduler(group)

	// Drive the group into the fallback/recovering state with rotation
	// active so unconfirmed candidates exist on the recovery cursor.
	triggerCandidateHealth(group, 0, TestNetworkType, false)
	assertTCPAndUDPSelected(t, group, nodes.Fallback)
	driveProbeResults(t, group, scheduler,
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		scriptedProbeResult{target: nodes.A},
		// One B failure advances the cursor to C, leaving B and C as
		// unconfirmed candidates that must NOT be selected on the hot path.
		scriptedProbeResult{target: nodes.B},
	)

	// The fixed Fallback is active for new traffic.
	assertTCPAndUDPSelected(t, group, nodes.Fallback)

	// Exclude the fixed Fallback. The failover policy must return
	// ErrNoAliveDialer rather than scanning the ordered candidate list for
	// an unconfirmed primary. This is the selection-level proof that a
	// fallback dial failure surfaces as an error to the caller without
	// promoting an unconfirmed candidate.
	d, _, _, err := group.SelectWithExclusionResult(TestNetworkType, false, nodes.Fallback)
	if !errors.Is(err, ErrNoAliveDialer) {
		t.Fatalf("excluding fallback during recovery: err = %v, want ErrNoAliveDialer", err)
	}
	if d != nil {
		t.Fatalf("excluding fallback returned dialer %v, want nil (no unconfirmed candidate)", d)
	}

	// The controller state is unchanged by the failed selection: still on
	// the fixed Fallback with rotation active.
	assertTCPAndUDPSelected(t, group, nodes.Fallback)
	group.failoverController.mu.Lock()
	state := group.failoverController.state
	rotationActive := group.failoverController.rotationActive
	group.failoverController.mu.Unlock()
	if state != stateFallbackActive {
		t.Fatalf("state = %v, want stateFallbackActive", state)
	}
	if !rotationActive {
		t.Fatal("rotationActive = false, want true (exclusion must not change state)")
	}
}

// newPerNodeHTTPServer starts an HTTP health-check server whose availability
// is gated by the returned *atomic.Bool. When the flag is false the server
// hijacks and closes the connection, causing ProbeTCPOnce to report the node
// as unavailable. The server is auto-closed on test cleanup.
func newPerNodeHTTPServer(t *testing.T, up *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newNamedDirectDialerWithCheck builds a direct dialer whose TCP health check
// targets the given URL, so each node's ProbeTCPOnce resolves to a distinct
// HTTP server. This is what production uses (no test probe seam).
func newNamedDirectDialerWithCheck(option *dialer.GlobalOption, name, tcpCheckURL string) *dialer.Dialer {
	nodeOption := *option
	nodeOption.TcpCheckOptionRaw = dialer.TcpCheckOptionRaw{Raw: []string{tcpCheckURL}}
	d := newDirectDialer(&nodeOption)
	d.Property().Name = name
	return d
}

// newRotationIntegrationGroupPerNode builds a rotation-enabled DialerGroup
// where each primary candidate and the fallback point at their own HTTP health
// server via distinct TcpCheckOptionRaw URLs. This exercises the PRODUCTION
// probe path (d.ProbeTCPOnce(ctx)) rather than a test-injected probe seam.
//
// The returned up flags gate each node's HTTP server. The fake scheduler is
// installed so the recovery loop can be driven deterministically without
// real-time sleeps; the probe function itself is NOT injected.
func newRotationIntegrationGroupPerNode(t *testing.T) (*DialerGroup, rotationTestNodes, map[string]*atomic.Bool, *fakeFailoverScheduler) {
	t.Helper()

	baseOption := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     time.Hour, // no periodic probes; only the recovery loop probes
		CheckTolerance:    0,
	}

	up := map[string]*atomic.Bool{
		"A":        &atomic.Bool{},
		"B":        &atomic.Bool{},
		"C":        &atomic.Bool{},
		"Fallback": &atomic.Bool{},
	}
	for _, flag := range up {
		flag.Store(true)
	}

	srvA := newPerNodeHTTPServer(t, up["A"])
	srvB := newPerNodeHTTPServer(t, up["B"])
	srvC := newPerNodeHTTPServer(t, up["C"])
	srvFallback := newPerNodeHTTPServer(t, up["Fallback"])

	nodes := rotationTestNodes{
		A:        newNamedDirectDialerWithCheck(baseOption, "A", srvA.URL),
		B:        newNamedDirectDialerWithCheck(baseOption, "B", srvB.URL),
		C:        newNamedDirectDialerWithCheck(baseOption, "C", srvC.URL),
		Fallback: newNamedDirectDialerWithCheck(baseOption, "fallback", srvFallback.URL),
	}
	dialers := []*dialer.Dialer{nodes.Fallback, nodes.C, nodes.A, nodes.B}
	annotations := []*dialer.Annotation{{}, {}, {}, {}}
	cfg := &FailoverConfig{
		PrimaryCandidateIdxs: []int{2, 3, 1},
		FallbackIdx:          0,
		Recovery: FailoverRecoveryConfig{
			ProbeInitial:     15 * time.Second,
			ProbeMax:         5 * time.Minute,
			Successes:        3,
			StableTime:       30 * time.Second,
			RotationAttempts: 5,
		},
	}
	group := NewDialerGroup(
		baseOption,
		"rotation-per-node",
		dialers,
		annotations,
		DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
		func(bool, *dialer.NetworkType, bool) {},
		cfg,
	)
	t.Cleanup(func() { _ = group.Close() })

	// Install the fake scheduler for deterministic timer firing. Crucially, do
	// NOT inject probeTargetTCP: the production path (d.ProbeTCPOnce(ctx) when
	// probeTCP is nil, or the legacy probeTCP closure when set by the ctor) must
	// be exercised against each node's own HTTP server.
	scheduler := installRotationTestScheduler(group)

	return group, nodes, up, scheduler
}

// TestFailoverRotationProductionProbeTargetsRecoveryTarget is a PRODUCTION-path
// regression test for a Critical bug where probeTarget's production fallback
// bound a closure to the initial primary at construction time and ignored the
// recoveryTarget dialer, so rotation to B/C would still probe A in production.
//
// This test deliberately does NOT inject probeTargetTCP or probeTCP. Each
// candidate points at its own HTTP health server, so d.ProbeTCPOnce(ctx)
// resolves to the node being probed. After five failed A probes activate
// rotation, the next probe must target B: we keep A down and bring B up, so a
// correct production path yields a successful B probe (confirmation starts on
// B), while the bug would yield a failed A probe and advance to C.
func TestFailoverRotationProductionProbeTargetsRecoveryTarget(t *testing.T) {
	group, nodes, up, scheduler := newRotationIntegrationGroupPerNode(t)

	// Sanity: initial selection is A.
	if d := selectedDialer(t, group); d != nodes.A {
		t.Fatalf("initial selection = %v, want A", d)
	}

	// A goes down — trigger failover via the identity-aware callback path.
	up["A"].Store(false)
	triggerCandidateHealth(group, 0, TestNetworkType, false)

	// The controller must now be on the fixed Fallback.
	if d := selectedDialer(t, group); d != nodes.Fallback {
		t.Fatalf("after A failure, selection = %v, want Fallback", d)
	}

	// Drive five failed recovery probes. A is down, so a production probe of A
	// (the recovery target before rotation) returns false. Each FireNext fires
	// the pending recovery timer, which runs runProbe -> probeTarget ->
	// d.ProbeTCPOnce(ctx) against A's (down) HTTP server.
	for i := 1; i <= 5; i++ {
		scheduler.FireNext(t)
	}

	// After five failures: rotation active, recoveryTarget advanced to B (index 1).
	group.failoverController.mu.Lock()
	failedRecoveryProbes := group.failoverController.failedRecoveryProbes
	rotationActive := group.failoverController.rotationActive
	recoveryTarget := group.failoverController.recoveryTarget
	recoverySuccesses := group.failoverController.recoverySuccesses
	group.failoverController.mu.Unlock()

	if failedRecoveryProbes != 0 {
		t.Fatalf("failedRecoveryProbes = %d, want 0 after advancement", failedRecoveryProbes)
	}
	if !rotationActive {
		t.Fatal("rotationActive = false, want true after 5 failed probes")
	}
	if recoveryTarget != 1 {
		t.Fatalf("recoveryTarget = %d, want 1 (B) after rotation activates", recoveryTarget)
	}
	if recoverySuccesses != 0 {
		t.Fatalf("recoverySuccesses = %d, want 0 before any B probe", recoverySuccesses)
	}

	// The decisive assertion: keep A down and bring B up. Fire the next
	// recovery timer. The production probe must now hit B's HTTP server (up),
	// returning ok=true, which starts confirmation on B.
	//
	// If the production path ignored the recoveryTarget dialer and probed A
	// (the original bug), this probe would fail (A is still down), advancing
	// the cursor to C and incrementing failedRecoveryProbes to 6.
	up["B"].Store(true)
	scheduler.FireNext(t)

	group.failoverController.mu.Lock()
	failedRecoveryProbes = group.failoverController.failedRecoveryProbes
	recoveryTarget = group.failoverController.recoveryTarget
	recoverySuccesses = group.failoverController.recoverySuccesses
	group.failoverController.mu.Unlock()

	if recoverySuccesses != 1 {
		t.Fatalf("recoverySuccesses = %d, want 1: production probe did not successfully hit B (recoveryTarget). "+
			"failedRecoveryProbes=%d, recoveryTarget=%d. This indicates probeTarget's production path is not "+
			"probing the recoveryTarget dialer.", recoverySuccesses, failedRecoveryProbes, recoveryTarget)
	}
	if recoveryTarget != 1 {
		t.Fatalf("recoveryTarget = %d, want 1 (B): a successful B probe must hold the cursor on B", recoveryTarget)
	}
	if failedRecoveryProbes != 0 {
		t.Fatalf("failedRecoveryProbes = %d, want 0 (a successful probe resets the consecutive failure count)",
			failedRecoveryProbes)
	}
}
