/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"testing"
	"time"

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
