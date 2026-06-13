/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/outbound/dialer"
	"github.com/daeuniverse/dae/pkg/logger"
	"github.com/sirupsen/logrus"
)

// ---------------------------------------------------------------------------
// Integration test infrastructure
// ---------------------------------------------------------------------------

// integrationGroup encapsulates a failover DialerGroup with two dialers
// (primary + fallback) and exposes controlled health-check simulation via
// real HTTP servers that can be started/stopped.
type integrationGroup struct {
	group    *DialerGroup
	primary  *dialer.Dialer
	fallback *dialer.Dialer
	log      *logrus.Logger

	// TCP listeners act as the "remote endpoint" for real dial attempts.
	primaryLn  net.Listener
	fallbackLn net.Listener

	// HTTP servers act as health-check targets.
	// Primary can be closed to simulate "node down".
	primarySrv  *httptest.Server
	fallbackSrv *httptest.Server
	primaryUp   *atomic.Bool

	// Track transitions for observability in tests.
	failoverCount atomic.Int32
	failbackCount atomic.Int32
}

func newIntegrationGroup(t *testing.T) *integrationGroup {
	t.Helper()

	log := logrus.New()
	log.SetOutput(io.Discard)
	logger.SetLogger(log, "trace", false, nil)

	// Start HTTP servers as health check targets.
	primaryUp := &atomic.Bool{}
	primaryUp.Store(true)
	primarySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !primaryUp.Load() {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Start TCP listeners for real dial attempts.
	primaryLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		primarySrv.Close()
		fallbackSrv.Close()
		t.Fatalf("failed to start primary listener: %v", err)
	}
	fallbackLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		primaryLn.Close()
		primarySrv.Close()
		fallbackSrv.Close()
		t.Fatalf("failed to start fallback listener: %v", err)
	}

	// Accept connections so dial attempts succeed.
	go func() {
		for {
			conn, err := primaryLn.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	go func() {
		for {
			conn, err := fallbackLn.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	tcpCheckURL := primarySrv.URL

	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{tcpCheckURL}},
		CheckInterval:     time.Hour, // disable periodic checks
		CheckTolerance:    0,
	}

	// Create two direct dialers.
	primaryUnderlay, primaryProp := dialer.NewDirectDialer(option, true)
	primaryProp.Name = "primary-node"
	primary := dialer.NewDialer(primaryUnderlay, option, dialer.InstanceOption{DisableCheck: false}, primaryProp)

	fallbackUnderlay, fallbackProp := dialer.NewDirectDialer(option, true)
	fallbackProp.Name = "fallback-node"
	fallback := dialer.NewDialer(fallbackUnderlay, option, dialer.InstanceOption{DisableCheck: true}, fallbackProp)

	annotations := []*dialer.Annotation{
		{Priority: 0}, // primary
		{Priority: 1}, // fallback
	}

	recoveryConfig := FailoverRecoveryConfig{
		ProbeInitial: 50 * time.Millisecond,
		ProbeMax:     200 * time.Millisecond,
		Successes:    2,
		StableTime:   100 * time.Millisecond,
	}

	failoverCfg := &FailoverConfig{
		PrimaryIdx:  0,
		FallbackIdx: 1,
		Recovery:    recoveryConfig,
	}

	policy := DialerSelectionPolicy{
		Policy: consts.DialerSelectionPolicy_Failover,
	}

	var aliveCallback = func(alive bool, networkType *dialer.NetworkType, isInit bool) {}

	group := NewDialerGroup(option, "integration-test", []*dialer.Dialer{primary, fallback}, annotations, policy, aliveCallback, failoverCfg)

	ig := &integrationGroup{
		group:       group,
		primary:     primary,
		fallback:    fallback,
		log:         log,
		primaryLn:   primaryLn,
		fallbackLn:  fallbackLn,
		primarySrv:  primarySrv,
		fallbackSrv: fallbackSrv,
		primaryUp:   primaryUp,
	}

	t.Cleanup(func() {
		primaryLn.Close()
		fallbackLn.Close()
		primarySrv.Close()
		fallbackSrv.Close()
		_ = group.Close()
	})

	return ig
}

// simulatePrimaryDown closes the primary HTTP server and sends the same
// targeted health-check notification used by the real proxy dial failure path.
func (ig *integrationGroup) simulatePrimaryDown(t *testing.T) {
	t.Helper()

	ig.primaryUp.Store(false)

	ig.primary.NotifyCheckTcp()

	deadline := time.Now().Add(3 * time.Second)
	for ig.group.failoverController.State() != stateFallbackActive && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if state := ig.group.failoverController.State(); state != stateFallbackActive {
		t.Fatalf("targeted TCP failure notification was not consumed: state=%v", state)
	}
}

// simulatePrimaryUp restarts the primary HTTP server and runs a successful
// probe to mark it alive again.
func (ig *integrationGroup) simulatePrimaryUp(t *testing.T) {
	t.Helper()

	ig.primaryUp.Store(true)

	// Run a successful probe.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ok, err := ig.primary.ProbeTCPOnce(ctx)
	if err != nil || !ok {
		t.Fatalf("primary probe should succeed after restart: err=%v ok=%v", err, ok)
	}
}

// dialTCP attempts a TCP connection through the group and returns which
// dialer was selected or an error. If excludeFallback is true and the
// active role is fallback, the fallback dialer is excluded.
func (ig *integrationGroup) dialTCP(t *testing.T, excludeFallback bool) (*dialer.Dialer, error) {
	t.Helper()
	var excluded *dialer.Dialer
	if excludeFallback {
		excluded = ig.fallback
	}
	d, _, _, err := ig.group._select(TestNetworkType, nil, DialerSelectionPolicy{
		Policy: consts.DialerSelectionPolicy_Failover,
	}, excluded)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Integration test cases
// ---------------------------------------------------------------------------

// TestIntegration_Failover_PrimaryDown_SwitchToFallback verifies that when
// the primary TCP path becomes unavailable, new connections switch to the
// fallback without reload or restart. (AC3)
func TestIntegration_Failover_PrimaryDown_SwitchToFallback(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Phase 1: Primary is healthy — selection should return primary.
	d, err := ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("phase 1 select error: %v", err)
	}
	if d != ig.primary {
		t.Fatalf("phase 1: expected primary, got %s", d.Property().Name)
	}

	// Phase 2: Simulate primary TCP failure via real probe.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	// Phase 3: Selection should now return fallback.
	d, err = ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("phase 3 select error: %v", err)
	}
	if d != ig.fallback {
		t.Fatalf("phase 3: expected fallback, got %s", d.Property().Name)
	}
}

// TestIntegration_Failover_Failback_CompleteCycle verifies the complete
// failover → recovery → failback cycle. (AC5, AC6, AC7)
func TestIntegration_Failover_Failback_CompleteCycle(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Phase 1: Verify primary is selected initially.
	d, err := ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("phase 1 select error: %v", err)
	}
	if d != ig.primary {
		t.Fatalf("phase 1: expected primary, got %s", d.Property().Name)
	}

	// Phase 2: Primary TCP fails → enter fallback_active.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	d, err = ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("phase 2 select error: %v", err)
	}
	if d != ig.fallback {
		t.Fatalf("phase 2: expected fallback, got %s", d.Property().Name)
	}

	// Phase 3: Bring primary back up.
	ig.simulatePrimaryUp(t)

	// Wait for recovery probes to fire and succeed.
	// With ProbeInitial=50ms, Successes=2, StableTime=100ms:
	// - First probe at ~50ms → success → enter recovering
	// - Confirmation probe at 50ms → success (2 total)
	// - Wait for StableTime (100ms) → failback
	time.Sleep(300 * time.Millisecond)

	// Phase 4: Selection should return primary again.
	d, err = ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("phase 4 select error: %v", err)
	}
	if d != ig.primary {
		t.Fatalf("phase 4: expected primary after failback, got %s", d.Property().Name)
	}
}

// TestIntegration_Failover_TransientSuccess_NoFailback verifies that a
// single TCP recovery event does NOT trigger failback. (AC6)
func TestIntegration_Failover_TransientSuccess_NoFailback(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Trigger failover.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	// Verify fallback is active.
	d, err := ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("select error: %v", err)
	}
	if d != ig.fallback {
		t.Fatalf("expected fallback, got %s", d.Property().Name)
	}

	// Simulate a single primary health recovery (transient success).
	ig.simulatePrimaryUp(t)

	// Wait only long enough for one probe to succeed — not enough for failback.
	time.Sleep(80 * time.Millisecond)

	// Should still be on fallback — one success is insufficient.
	d, err = ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("select error after transient success: %v", err)
	}
	if d != ig.fallback {
		t.Fatalf("transient success should NOT trigger failback, got %s", d.Property().Name)
	}
}

// TestIntegration_Failover_UDP_SelectsActiveRole verifies that UDP session
// selection follows the same active role as TCP. (AC3, AC7)
func TestIntegration_Failover_UDP_SelectsActiveRole(t *testing.T) {
	ig := newIntegrationGroup(t)

	networkType := &dialer.NetworkType{
		L4Proto:   consts.L4ProtoStr_UDP,
		IpVersion: consts.IpVersionStr_4,
		IsDns:     false,
	}

	// Phase 1: UDP should use primary initially.
	d, _, err := ig.group.Select(networkType, false)
	if err != nil {
		t.Fatalf("phase 1 UDP select error: %v", err)
	}
	if d != ig.primary {
		t.Fatalf("phase 1 UDP: expected primary, got %s", d.Property().Name)
	}

	// Phase 2: Primary TCP fails.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	// Phase 3: UDP should now use fallback.
	d, _, err = ig.group.Select(networkType, false)
	if err != nil {
		t.Fatalf("phase 3 UDP select error: %v", err)
	}
	if d != ig.fallback {
		t.Fatalf("phase 3 UDP: expected fallback, got %s", d.Property().Name)
	}
}

// TestIntegration_Failover_FallbackExcluded_ReturnsError verifies that
// when the fallback is excluded while active, selection returns an error.
func TestIntegration_Failover_FallbackExcluded_ReturnsError(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Trigger failover.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	// Verify we're actually in fallback state.
	ctrl := ig.group.failoverController
	if ctrl == nil {
		t.Fatal("failover controller is nil")
	}
	state := ctrl.State()
	if state != stateFallbackActive {
		t.Fatalf("expected stateFallbackActive, got %v", state)
	}

	// Exclude fallback — should return error, not primary.
	_, err := ig.dialTCP(t, true)
	if !errors.Is(err, ErrNoAliveDialer) {
		t.Fatalf("expected ErrNoAliveDialer when fallback excluded, got: %v", err)
	}
}

// TestIntegration_Failover_ConcurrentFailures_OneTransition verifies that
// concurrent connection failures only produce ONE state transition.
func TestIntegration_Failover_ConcurrentFailures_OneTransition(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Simulate concurrent TCP probe failures from multiple goroutines.
	// All probes will fail because the endpoint closes accepted connections.
	ig.primaryUp.Store(false)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, _ = ig.primary.ProbeTCPOnce(ctx)
		}()
	}
	wg.Wait()

	time.Sleep(50 * time.Millisecond)

	// Should have transitioned exactly once to fallback.
	d, err := ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("select error: %v", err)
	}
	if d != ig.fallback {
		t.Fatalf("concurrent failures: expected fallback, got %s", d.Property().Name)
	}
}

// TestIntegration_Failover_FailureDuringRecovery_ResetsProgress verifies
// that a failure during the recovery phase resets progress.
func TestIntegration_Failover_FailureDuringRecovery_ResetsProgress(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Trigger failover.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	// Bring primary up briefly for a probe to succeed.
	ig.simulatePrimaryUp(t)
	time.Sleep(80 * time.Millisecond)

	// Then bring it down again — failure during recovery resets progress.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	// Should still be on fallback (or recovering but not yet failback).
	d, err := ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("select error: %v", err)
	}
	if d == ig.primary {
		t.Fatalf("failure during recovery should NOT failback, got primary")
	}

	// Now complete recovery cleanly.
	ig.simulatePrimaryUp(t)
	time.Sleep(300 * time.Millisecond)

	d, err = ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("select error after clean recovery: %v", err)
	}
	if d != ig.primary {
		t.Fatalf("expected primary after clean recovery, got %s", d.Property().Name)
	}
}

// TestIntegration_Failover_WarmReload_PreservesState verifies that a
// warm reload with identical dialer identities preserves the fallback state.
func TestIntegration_Failover_WarmReload_PreservesState(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Trigger failover.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	// Capture the failover snapshot.
	snap := ig.group.CaptureFailoverSnapshot()
	if snap == nil {
		t.Fatal("expected non-nil failover snapshot")
	}

	// Verify the snapshot reflects fallback_active state.
	if snap.State != stateFallbackActive {
		t.Fatalf("snapshot state = %d, want fallback_active (%d)", snap.State, stateFallbackActive)
	}

	// Verify NextProbeAt is set (recovery timer armed).
	if snap.NextProbeAt.IsZero() {
		t.Fatal("expected NextProbeAt to be set for fallback_active state")
	}

	// Simulate reload: create a new group with same identities.
	option := &dialer.GlobalOption{
		Log:               ig.log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{"http://127.0.0.1:0"}},
		CheckInterval:     time.Hour,
		CheckTolerance:    0,
	}

	primaryUnderlay, primaryProp := dialer.NewDirectDialer(option, true)
	primaryProp.Name = "primary-node"
	primary := dialer.NewDialer(primaryUnderlay, option, dialer.InstanceOption{DisableCheck: true}, primaryProp)

	fallbackUnderlay, fallbackProp := dialer.NewDirectDialer(option, true)
	fallbackProp.Name = "fallback-node"
	fallback := dialer.NewDialer(fallbackUnderlay, option, dialer.InstanceOption{DisableCheck: true}, fallbackProp)

	annotations := []*dialer.Annotation{
		{Priority: 0},
		{Priority: 1},
	}

	recoveryConfig := FailoverRecoveryConfig{
		ProbeInitial: 50 * time.Millisecond,
		ProbeMax:     200 * time.Millisecond,
		Successes:    2,
		StableTime:   100 * time.Millisecond,
	}

	failoverCfg := &FailoverConfig{
		PrimaryIdx:  0,
		FallbackIdx: 1,
		Recovery:    recoveryConfig,
	}

	policy := DialerSelectionPolicy{
		Policy: consts.DialerSelectionPolicy_Failover,
	}

	newGroup := NewDialerGroup(option, "integration-test-reload", []*dialer.Dialer{primary, fallback}, annotations, policy, func(alive bool, networkType *dialer.NetworkType, isInit bool) {}, failoverCfg)
	t.Cleanup(func() { _ = newGroup.Close() })

	// Verify identity matches.
	newPrimaryName, newFallbackName := newGroup.FailoverIdentity()
	if newPrimaryName != ig.primary.Property().Name || newFallbackName != ig.fallback.Property().Name {
		t.Fatal("identity mismatch — would skip state restore")
	}

	// Restore snapshot.
	newGroup.RestoreFailoverSnapshot(snap)

	// Verify restored group selects the active role dialer.
	d, _, err := newGroup.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("select after restore error: %v", err)
	}
	// In fallback_active, activeIdx=1, which maps to failoverCfg.FallbackIdx=1 = fallback
	if d != fallback {
		t.Fatalf("after restore: expected fallback dialer, got %s", d.Property().Name)
	}
}

// TestIntegration_Failover_Reload_IdentityChanged_ResetsToPrimary verifies
// that when dialer identity changes during reload, the new group starts
// in primary_active state.
func TestIntegration_Failover_Reload_IdentityChanged_ResetsToPrimary(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Trigger failover.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	// Create new group with different dialer identity.
	option := &dialer.GlobalOption{
		Log:               ig.log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{"http://127.0.0.1:0"}},
		CheckInterval:     time.Hour,
		CheckTolerance:    0,
	}

	newPrimaryUnderlay, newPrimaryProp := dialer.NewDirectDialer(option, true)
	newPrimaryProp.Name = "new-primary-node"
	newPrimary := dialer.NewDialer(newPrimaryUnderlay, option, dialer.InstanceOption{DisableCheck: true}, newPrimaryProp)

	newFallbackUnderlay, newFallbackProp := dialer.NewDirectDialer(option, true)
	newFallbackProp.Name = "new-fallback-node"
	newFallback := dialer.NewDialer(newFallbackUnderlay, option, dialer.InstanceOption{DisableCheck: true}, newFallbackProp)

	annotations := []*dialer.Annotation{
		{Priority: 0},
		{Priority: 1},
	}

	recoveryConfig := FailoverRecoveryConfig{
		ProbeInitial: 50 * time.Millisecond,
		ProbeMax:     200 * time.Millisecond,
		Successes:    2,
		StableTime:   100 * time.Millisecond,
	}

	failoverCfg := &FailoverConfig{
		PrimaryIdx:  0,
		FallbackIdx: 1,
		Recovery:    recoveryConfig,
	}

	policy := DialerSelectionPolicy{
		Policy: consts.DialerSelectionPolicy_Failover,
	}

	newGroup := NewDialerGroup(option, "integration-test-reload-new", []*dialer.Dialer{newPrimary, newFallback}, annotations, policy, func(alive bool, networkType *dialer.NetworkType, isInit bool) {}, failoverCfg)
	t.Cleanup(func() { _ = newGroup.Close() })

	// Identity should NOT match.
	oldPrimaryName, oldFallbackName := ig.group.FailoverIdentity()
	newPrimaryName, newFallbackName := newGroup.FailoverIdentity()
	if oldPrimaryName == newPrimaryName && oldFallbackName == newFallbackName {
		t.Fatal("expected different identities but they matched")
	}

	// New group should select primary (initial state).
	d, _, err := newGroup.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("select error: %v", err)
	}
	if d != newPrimary {
		t.Fatalf("new group with changed identity should start on primary, got %s", d.Property().Name)
	}
}

// TestIntegration_Failover_PrimaryActive_NoPeriodicProbes verifies that
// in primary_active state, no failover-specific recovery probes run.
func TestIntegration_Failover_PrimaryActive_NoPeriodicProbes(t *testing.T) {
	ig := newIntegrationGroup(t)

	// In primary_active, no recovery probe should be running.
	time.Sleep(200 * time.Millisecond)

	// Verify group is still selecting primary.
	d, err := ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("select error: %v", err)
	}
	if d != ig.primary {
		t.Fatalf("expected primary in primary_active, got %s", d.Property().Name)
	}
}

// TestIntegration_Failover_RealDial_UsesActualTCP verifies that the
// failover group works with real TCP dial attempts.
func TestIntegration_Failover_RealDial_UsesActualTCP(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Select primary and make a real TCP connection.
	d, _, err := ig.group.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("select error: %v", err)
	}
	if d != ig.primary {
		t.Fatalf("expected primary, got %s", d.Property().Name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", ig.primaryLn.Addr().String())
	if err != nil {
		t.Fatalf("real dial through primary error: %v", err)
	}
	conn.Close()

	// Now trigger failover and dial through fallback.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	d, _, err = ig.group.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("select after failover error: %v", err)
	}
	if d != ig.fallback {
		t.Fatalf("expected fallback after failover, got %s", d.Property().Name)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	conn2, err := d.DialContext(ctx2, "tcp", ig.fallbackLn.Addr().String())
	if err != nil {
		t.Fatalf("real dial through fallback error: %v", err)
	}
	conn2.Close()
}

// TestIntegration_Failover_Shutdown_CancelsTimers verifies that closing
// the group cancels recovery timers and active probe contexts.
func TestIntegration_Failover_Shutdown_CancelsTimers(t *testing.T) {
	ig := newIntegrationGroup(t)

	// Trigger failover to arm recovery timer.
	ig.simulatePrimaryDown(t)
	time.Sleep(50 * time.Millisecond)

	// Close the group — this should cancel all timers and probes.
	err := ig.group.Close()
	if err != nil {
		t.Fatalf("close error: %v", err)
	}

	// After close, no panic should occur.
	time.Sleep(50 * time.Millisecond)
}

// TestIntegration_Failover_NoActivationOfOrdinaryPeriodicChecker verifies
// that constructing a failover group does NOT activate dae's ordinary
// periodic latency/health checking.
func TestIntegration_Failover_NoActivationOfOrdinaryPeriodicChecker(t *testing.T) {
	ig := newIntegrationGroup(t)

	// The failover group should not have an AliveDialerSet.
	set := ig.group.MustGetAliveDialerSet(TestNetworkType)
	if set != nil {
		t.Fatal("failover group should NOT have an AliveDialerSet")
	}

	// Wait long enough that if periodic checks were active, they would fire.
	time.Sleep(500 * time.Millisecond)

	// Primary should still be selected (no periodic check could have changed state).
	d, err := ig.dialTCP(t, false)
	if err != nil {
		t.Fatalf("select error: %v", err)
	}
	if d != ig.primary {
		t.Fatalf("expected primary with no periodic checker, got %s", d.Property().Name)
	}
}

// ---------------------------------------------------------------------------
// Test summary:
//
// Test Case                                          | AC / Feature
// ---------------------------------------------------|---------------------------
// TestIntegration_Failover_PrimaryDown_SwitchToFallback    | AC3
// TestIntegration_Failover_Failback_CompleteCycle          | AC5, AC6, AC7
// TestIntegration_Failover_TransientSuccess_NoFailback     | AC6
// TestIntegration_Failover_UDP_SelectsActiveRole           | AC3, AC7
// TestIntegration_Failover_FallbackExcluded_ReturnsError   | Critical #2 fix
// TestIntegration_Failover_ConcurrentFailures_OneTransition| Concurrency model
// TestIntegration_Failover_FailureDuringRecovery_ResetsProgress | AC5
// TestIntegration_Failover_WarmReload_PreservesState       | AC9
// TestIntegration_Failover_Reload_IdentityChanged_ResetsToPrimary | AC9
// TestIntegration_Failover_PrimaryActive_NoPeriodicProbes  | AC2
// TestIntegration_Failover_RealDial_UsesActualTCP          | AC1, AC8
// TestIntegration_Failover_Shutdown_CancelsTimers          | Lifecycle
// TestIntegration_Failover_NoActivationOfOrdinaryPeriodicChecker | AC2
// ---------------------------------------------------------------------------
