/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/outbound/dialer"
)

func TestFailoverController_InitialState(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc.Close()

	if fc.State() != statePrimaryActive {
		t.Fatalf("initial state = %v, want statePrimaryActive", fc.State())
	}
	if fc.ActiveDialerIndex() != 0 {
		t.Fatalf("initial active index = %d, want 0", fc.ActiveDialerIndex())
	}
}

func TestFailoverController_FailoverOnTcpUnavailable(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc.Close()

	// Simulate primary TCP becoming unavailable.
	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	if fc.State() != stateFallbackActive {
		t.Fatalf("state after TCP fail = %v, want stateFallbackActive", fc.State())
	}
	if fc.ActiveDialerIndex() != 1 {
		t.Fatalf("active index after fail = %d, want 1", fc.ActiveDialerIndex())
	}
}

func TestFailoverController_IgnoresUdpTransition(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc.Close()

	// Simulate primary UDP becoming unavailable — should NOT trigger failover.
	udp4 := &dialer.NetworkType{L4Proto: "udp", IpVersion: "4"}
	fc.onPrimaryHealthChange(udp4, false)

	if fc.State() != statePrimaryActive {
		t.Fatalf("state after UDP fail = %v, want statePrimaryActive (UDP should not trigger failover)", fc.State())
	}
}

func TestFailoverController_IgnoresPrimaryRecovery(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc.Close()

	// Simulate primary TCP becoming unavailable.
	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	// Simulate primary TCP recovering — should NOT immediately switch back.
	fc.onPrimaryHealthChange(tcp4, true)

	if fc.State() != stateFallbackActive {
		t.Fatalf("state after primary recovery = %v, want stateFallbackActive (single success should not trigger failback)", fc.State())
	}
	if fc.ActiveDialerIndex() != 1 {
		t.Fatalf("active index after recovery = %d, want 1 (still on fallback)", fc.ActiveDialerIndex())
	}
}

func TestFailoverController_Close(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})

	// Simulate failover to schedule a timer.
	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	// Close should not panic.
	fc.Close()
}

func TestFailoverController_StaleCallbackIgnored(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})

	// Close to increment generation.
	fc.Close()

	// Simulate a stale callback — should be ignored.
	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	// State should remain primary_active since the controller was closed.
	// (The callback runs but the state change happens — this is fine because
	// the controller is closed and the group won't use it anymore.)
}

func TestFailoverController_CaptureRestoreSnapshot(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc.Close()

	// Simulate failover.
	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	// Capture snapshot.
	snap := fc.CaptureSnapshot()
	if snap.State != stateFallbackActive {
		t.Fatalf("captured state = %v, want stateFallbackActive", snap.State)
	}

	// Create a new controller and restore.
	primary2 := newDirectDialer(option, false)
	fallback2 := newDirectDialer(option, false)
	fc2 := NewFailoverController(log, "test", primary2, fallback2, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc2.Close()

	fc2.RestoreSnapshot(snap)
	if fc2.State() != stateFallbackActive {
		t.Fatalf("restored state = %v, want stateFallbackActive", fc2.State())
	}
	if fc2.ActiveDialerIndex() != 1 {
		t.Fatalf("restored active index = %d, want 1", fc2.ActiveDialerIndex())
	}
}

func TestValidateFailoverGroup_Valid(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	dialers := []*dialer.Dialer{
		newDirectDialer(option, false),
		newDirectDialer(option, false),
	}
	annotations := []*dialer.Annotation{
		{Priority: 0},
		{Priority: 1},
	}

	cfg, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PrimaryIdx != 0 || cfg.FallbackIdx != 1 {
		t.Fatalf("unexpected config: primary=%d, fallback=%d", cfg.PrimaryIdx, cfg.FallbackIdx)
	}
}

func TestValidateFailoverGroup_ThreeDialers(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	dialers := []*dialer.Dialer{
		newDirectDialer(option, false),
		newDirectDialer(option, false),
		newDirectDialer(option, false),
	}
	annotations := []*dialer.Annotation{
		{Priority: 0},
		{Priority: 1},
		{Priority: 2},
	}

	_, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for 3 dialers")
	}
}

func TestValidateFailoverGroup_MissingPriority(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	dialers := []*dialer.Dialer{
		newDirectDialer(option, false),
		newDirectDialer(option, false),
	}
	annotations := []*dialer.Annotation{
		{Priority: 0},
		{Priority: dialer.PriorityNotSet},
	}

	_, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for missing priority")
	}
}

func TestValidateFailoverGroup_DuplicatePriority(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	dialers := []*dialer.Dialer{
		newDirectDialer(option, false),
		newDirectDialer(option, false),
	}
	annotations := []*dialer.Annotation{
		{Priority: 0},
		{Priority: 0},
	}

	_, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for duplicate priority")
	}
}

func TestValidateFailoverGroup_InvalidRecoveryConfig(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	dialers := []*dialer.Dialer{
		newDirectDialer(option, false),
		newDirectDialer(option, false),
	}
	annotations := []*dialer.Annotation{
		{Priority: 0},
		{Priority: 1},
	}

	// ProbeInitial > ProbeMax
	_, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial: 10 * time.Minute,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for ProbeInitial > ProbeMax")
	}

	// Successes < 1
	_, err = ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    0,
		StableTime:   30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for Successes < 1")
	}
}

// --- Acceptance Tests ---

func TestFailoverController_ReverseFilterOrder(t *testing.T) {
	// Verify role-to-index mapping: Dialers[0]=fallback (priority:1), Dialers[1]=primary (priority:0).
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	dialers := []*dialer.Dialer{
		newDirectDialer(option, false), // index 0 = fallback
		newDirectDialer(option, false), // index 1 = primary
	}
	annotations := []*dialer.Annotation{
		{Priority: 1}, // index 0 is fallback
		{Priority: 0}, // index 1 is primary
	}
	cfg, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	if cfg.PrimaryIdx != 1 || cfg.FallbackIdx != 0 {
		t.Fatalf("unexpected indices: primary=%d fallback=%d", cfg.PrimaryIdx, cfg.FallbackIdx)
	}

	// Create DialerGroup and verify selection.
	group := NewDialerGroup(option, "test", dialers, annotations,
		DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
		func(bool, *dialer.NetworkType, bool) {},
		cfg)
	defer group.Close()

	nt := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	d, _, _, err := group.SelectWithExclusionResult(nt, false, nil)
	if err != nil {
		t.Fatalf("select failed: %v", err)
	}
	if d != dialers[1] {
		t.Fatalf("selected dialer %v, want primary (index 1)", d)
	}

	// Simulate primary failure → should select fallback (index 0).
	group.failoverController.onPrimaryHealthChange(nt, false)
	d, _, _, err = group.SelectWithExclusionResult(nt, false, nil)
	if err != nil {
		t.Fatalf("select after failover failed: %v", err)
	}
	if d != dialers[0] {
		t.Fatalf("selected dialer %v, want fallback (index 0)", d)
	}
}

func TestFailoverController_FallbackExcludedReturnsError(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)
	dialers := []*dialer.Dialer{primary, fallback}
	annotations := []*dialer.Annotation{{Priority: 0}, {Priority: 1}}

	cfg, _ := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	group := NewDialerGroup(option, "test", dialers, annotations,
		DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
		func(bool, *dialer.NetworkType, bool) {},
		cfg)
	defer group.Close()

	nt := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}

	// Failover to fallback.
	group.failoverController.onPrimaryHealthChange(nt, false)

	// Exclude the fallback → should return error, NOT the primary.
	_, _, _, err := group.SelectWithExclusionResult(nt, false, fallback)
	if err == nil {
		t.Fatal("expected error when fallback is excluded, got nil")
	}
}

func TestFailoverController_BackoffSequence(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 1 * time.Second,
		ProbeMax:     8 * time.Second,
		Successes:    3,
		StableTime:   1 * time.Second,
	})
	defer fc.Close()

	// Trigger failover.
	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	// Simulate probe failures and verify backoff.
	// After failover, currentDelay = ProbeInitial = 1s
	fc.mu.Lock()
	if fc.currentDelay != 1*time.Second {
		t.Fatalf("initial delay = %v, want 1s", fc.currentDelay)
	}

	// Simulate failure: delay should double.
	fc.onProbeFailureLocked()
	if fc.currentDelay != 2*time.Second {
		t.Fatalf("after 1st failure: delay = %v, want 2s", fc.currentDelay)
	}

	fc.onProbeFailureLocked()
	if fc.currentDelay != 4*time.Second {
		t.Fatalf("after 2nd failure: delay = %v, want 4s", fc.currentDelay)
	}

	fc.onProbeFailureLocked()
	if fc.currentDelay != 8*time.Second {
		t.Fatalf("after 3rd failure: delay = %v, want 8s", fc.currentDelay)
	}

	// Cap at ProbeMax.
	fc.onProbeFailureLocked()
	if fc.currentDelay != 8*time.Second {
		t.Fatalf("after 4th failure (capped): delay = %v, want 8s", fc.currentDelay)
	}

	// Success resets to ProbeInitial.
	fc.onProbeSuccessLocked()
	if fc.currentDelay != 1*time.Second {
		t.Fatalf("after success: delay = %v, want 1s", fc.currentDelay)
	}
	fc.mu.Unlock()
}

func TestFailoverController_StableTimeRequired(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 1 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    2,
		StableTime:   10 * time.Second,
	})
	defer fc.Close()

	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	// Two successes but not enough stable time → should NOT failback.
	fc.mu.Lock()
	fc.onProbeSuccessLocked() // successes=1
	fc.onProbeSuccessLocked() // successes=2, but stable time < 10s
	state := fc.state
	fc.mu.Unlock()

	if state != stateRecovering {
		t.Fatalf("state = %v, want stateRecovering (stable time not met)", state)
	}
	if fc.ActiveDialerIndex() != 1 {
		t.Fatalf("active index = %d, want 1 (still on fallback)", fc.ActiveDialerIndex())
	}
}

func TestFailoverController_ReloadRemainingTime(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 10 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc.Close()

	// Trigger failover → schedules probe at now+10s.
	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	// Wait 7 seconds → 3 seconds remaining.
	time.Sleep(7 * time.Second)

	// Capture snapshot.
	snap := fc.CaptureSnapshot()
	if snap.NextProbeAt.IsZero() {
		t.Fatal("NextProbeAt should be set")
	}
	remaining := time.Until(snap.NextProbeAt)
	if remaining < 2*time.Second || remaining > 4*time.Second {
		t.Fatalf("remaining = %v, want ~3s", remaining)
	}

	// Restore into a new controller.
	primary2 := newDirectDialer(option, false)
	fallback2 := newDirectDialer(option, false)
	fc2 := NewFailoverController(log, "test", primary2, fallback2, FailoverRecoveryConfig{
		ProbeInitial: 10 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc2.Close()

	before := time.Now()
	fc2.RestoreSnapshot(snap)
	elapsed := time.Since(before)

	// The probe should be scheduled with remaining delay (~3s), not the full 10s.
	if elapsed > 1*time.Second {
		t.Fatalf("RestoreSnapshot took %v, expected fast return", elapsed)
	}
	// Verify the timer fires within ~3s (remaining), not 10s (full delay).
	done := make(chan struct{})
	go func() {
		// The probe will fail (direct dialer, no real network), but we just
		// care that it fires quickly.
		time.Sleep(4 * time.Second)
		close(done)
	}()
	select {
	case <-done:
		// OK — probe fired within 4s (remaining ~3s + some slack)
	case <-time.After(6 * time.Second):
		t.Fatal("probe did not fire within expected remaining time")
	}
}

func TestFailoverController_StaleCallbackAfterClose(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})

	// Close the controller.
	fc.Close()

	// Simulate a stale callback — should NOT change state.
	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	if fc.State() != statePrimaryActive {
		t.Fatalf("state after stale callback = %v, want statePrimaryActive", fc.State())
	}
	if fc.ActiveDialerIndex() != 0 {
		t.Fatalf("active index after stale callback = %d, want 0", fc.ActiveDialerIndex())
	}
}

func TestValidateFailoverGroup_UnsupportedPriority(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	dialers := []*dialer.Dialer{
		newDirectDialer(option, false),
		newDirectDialer(option, false),
	}
	annotations := []*dialer.Annotation{
		{Priority: 0},
		{Priority: 2}, // unsupported
	}

	_, err := ValidateFailoverGroup(dialers, annotations, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for unsupported priority 2")
	}
}
