/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
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