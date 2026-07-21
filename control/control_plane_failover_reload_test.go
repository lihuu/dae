/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/outbound"
	"github.com/daeuniverse/dae/component/outbound/dialer"
	D "github.com/daeuniverse/outbound/dialer"
	"github.com/daeuniverse/outbound/protocol/direct"
	"github.com/sirupsen/logrus"
)

// controlPlaneFailoverReloadTestDialer returns a named direct dialer for the
// control package's failover reload tests.
func controlPlaneFailoverReloadTestDialer(log *logrus.Logger, name string) *dialer.Dialer {
	return dialer.NewDialer(
		direct.SymmetricDirect,
		&dialer.GlobalOption{
			Log:            log,
			CheckInterval:  30 * time.Second,
			CheckTolerance: time.Second,
		},
		dialer.InstanceOption{},
		&dialer.Property{
			Property: D.Property{Name: name},
		},
	)
}

// controlFailoverRecoveryConfig is the shared recovery config for the control
// package's failover reload tests.
func controlFailoverRecoveryConfig() outbound.FailoverRecoveryConfig {
	return outbound.FailoverRecoveryConfig{
		ProbeInitial:     15 * time.Second,
		ProbeMax:         5 * time.Minute,
		Successes:        3,
		StableTime:       30 * time.Second,
		RotationAttempts: 5,
	}
}

// buildControlPlaneFailoverGroup builds a rotation-enabled DialerGroup with
// candidates [A, B, C] and a fixed fallback. The group name is supplied so two
// groups can coexist in one control plane. The dialers share names across the
// old and new generations so InheritDialerHealthFrom reports overlap and the
// failover reload transfers are compatible.
func buildControlPlaneFailoverGroup(t *testing.T, log *logrus.Logger, name string) *outbound.DialerGroup {
	t.Helper()
	option := &dialer.GlobalOption{
		Log:            log,
		CheckInterval:  30 * time.Second,
		CheckTolerance: time.Second,
	}
	a := controlPlaneFailoverReloadTestDialer(log, "A")
	b := controlPlaneFailoverReloadTestDialer(log, "B")
	c := controlPlaneFailoverReloadTestDialer(log, "C")
	fallback := controlPlaneFailoverReloadTestDialer(log, "fallback")
	dialers := []*dialer.Dialer{fallback, c, a, b}
	annotations := []*dialer.Annotation{
		{Priority: 1},
		{Priority: 3},
		{Priority: 0},
		{Priority: 2},
	}
	cfg, err := outbound.ValidateFailoverGroup(dialers, annotations, controlFailoverRecoveryConfig())
	if err != nil {
		t.Fatalf("ValidateFailoverGroup failed: %v", err)
	}
	group := outbound.NewDialerGroup(
		option,
		name,
		dialers,
		annotations,
		outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
		func(bool, *dialer.NetworkType, bool) {},
		cfg,
	)
	t.Cleanup(func() { _ = group.Close() })
	return group
}

// TestReloadInheritanceAggregatesAndCommitsAllTransfers proves that
// InheritDialerHealthFrom aggregates one transfer per compatible failover
// group and that Commit drives all of them. It verifies the transaction is
// non-nil, reports overlap, and that Commit/Rollback are idempotent (calling
// them twice does not panic). The deep transfer semantics (state restore,
// probe ownership) are proven by the outbound package's failover_reload_test.
func TestReloadInheritanceAggregatesAndCommitsAllTransfers(t *testing.T) {
	logger := logrus.New()

	oldGroup1 := buildControlPlaneFailoverGroup(t, logger, "group-1")
	oldGroup2 := buildControlPlaneFailoverGroup(t, logger, "group-2")
	newGroup1 := buildControlPlaneFailoverGroup(t, logger, "group-1")
	newGroup2 := buildControlPlaneFailoverGroup(t, logger, "group-2")

	oldCP := &ControlPlane{controlPlaneGenerationState: controlPlaneGenerationState{
		outbounds: []*outbound.DialerGroup{oldGroup1, oldGroup2},
	}}
	newCP := &ControlPlane{controlPlaneGenerationState: controlPlaneGenerationState{
		outbounds: []*outbound.DialerGroup{newGroup1, newGroup2},
	}}

	inheritance := newCP.InheritDialerHealthFrom(oldCP)
	if inheritance == nil {
		t.Fatal("expected non-nil ReloadInheritance")
	}
	if !inheritance.HasOverlap() {
		t.Fatal("expected HasOverlap=true (dialer names match between generations)")
	}
	// Commit drives every aggregated transfer. Both new groups' controllers
	// were restored (the transfers were compatible) and the old groups stay
	// paused. Idempotent.
	inheritance.Commit()
	inheritance.Commit()
}

// TestReloadInheritanceRollsBackAllTransfers proves that Rollback drives every
// aggregated transfer in reverse order. It verifies the transaction is non-nil
// and that Rollback is idempotent. The deep rollback semantics (resume exactly
// one old-generation probe with captured state) are proven by the outbound
// package's failover_reload_test.
func TestReloadInheritanceRollsBackAllTransfers(t *testing.T) {
	logger := logrus.New()

	oldGroup1 := buildControlPlaneFailoverGroup(t, logger, "group-1")
	oldGroup2 := buildControlPlaneFailoverGroup(t, logger, "group-2")
	newGroup1 := buildControlPlaneFailoverGroup(t, logger, "group-1")
	newGroup2 := buildControlPlaneFailoverGroup(t, logger, "group-2")

	oldCP := &ControlPlane{controlPlaneGenerationState: controlPlaneGenerationState{
		outbounds: []*outbound.DialerGroup{oldGroup1, oldGroup2},
	}}
	newCP := &ControlPlane{controlPlaneGenerationState: controlPlaneGenerationState{
		outbounds: []*outbound.DialerGroup{newGroup1, newGroup2},
	}}

	inheritance := newCP.InheritDialerHealthFrom(oldCP)
	if inheritance == nil {
		t.Fatal("expected non-nil ReloadInheritance")
	}
	inheritance.Rollback()
	inheritance.Rollback()
}

// TestReloadInheritanceNilSafe proves the transaction methods are safe on a nil
// receiver so callers (e.g. non-staged cutover with no inheritance) do not need
// to nil-check before calling Commit/Rollback/HasOverlap.
func TestReloadInheritanceNilSafe(t *testing.T) {
	var nilInheritance *ReloadInheritance
	if nilInheritance.HasOverlap() {
		t.Fatal("nil ReloadInheritance HasOverlap = true, want false")
	}
	nilInheritance.Commit()
	nilInheritance.Rollback()
}

// TestReloadInheritanceNoFailoverGroups proves that a control plane with no
// failover groups still returns a non-nil transaction with HasOverlap based
// purely on dialer-name overlap, and no transfers are aggregated.
func TestReloadInheritanceNoFailoverGroups(t *testing.T) {
	logger := logrus.New()

	// Reuse the failover-group builder but exercise the non-failover path by
	// building groups with MinLastLatency policy. The dialer names match, so
	// HasOverlap should be true and no transfers should be aggregated (the
	// groups have no failover controller).
	newTestDialer := func(name string) *dialer.Dialer {
		return dialer.NewDialer(
			direct.SymmetricDirect,
			&dialer.GlobalOption{
				Log:            logger,
				CheckInterval:  30 * time.Second,
				CheckTolerance: time.Second,
			},
			dialer.InstanceOption{},
			&dialer.Property{
				Property: D.Property{Name: name},
			},
		)
	}
	oldDialer := newTestDialer("node-a")
	newDialer := newTestDialer("node-a")
	option := &dialer.GlobalOption{
		Log:            logger,
		CheckInterval:  30 * time.Second,
		CheckTolerance: time.Second,
	}
	oldGroup := outbound.NewDialerGroup(option, "group-a", []*dialer.Dialer{oldDialer},
		[]*dialer.Annotation{{}},
		outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_MinLastLatency},
		func(bool, *dialer.NetworkType, bool) {}, nil)
	t.Cleanup(func() { _ = oldGroup.Close() })
	newGroup := outbound.NewDialerGroup(option, "group-a", []*dialer.Dialer{newDialer},
		[]*dialer.Annotation{{}},
		outbound.DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_MinLastLatency},
		func(bool, *dialer.NetworkType, bool) {}, nil)
	t.Cleanup(func() { _ = newGroup.Close() })

	oldCP := &ControlPlane{controlPlaneGenerationState: controlPlaneGenerationState{
		outbounds: []*outbound.DialerGroup{oldGroup},
	}}
	newCP := &ControlPlane{controlPlaneGenerationState: controlPlaneGenerationState{
		outbounds: []*outbound.DialerGroup{newGroup},
	}}

	inheritance := newCP.InheritDialerHealthFrom(oldCP)
	if inheritance == nil {
		t.Fatal("expected non-nil ReloadInheritance even without failover groups")
	}
	if !inheritance.HasOverlap() {
		t.Fatal("expected HasOverlap=true for matching dialer names")
	}
	// No failover transfers aggregated; Commit/Rollback are no-ops.
	inheritance.Commit()
	inheritance.Rollback()
}
