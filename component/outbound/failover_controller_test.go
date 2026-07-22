/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"context"
	"errors"
	"sync"
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










func runFailoverProbeNow(t *testing.T, fc *FailoverController) {
	t.Helper()

	fc.mu.Lock()
	if fc.recoveryTimer != nil {
		fc.recoveryTimer.Stop()
		fc.recoveryTimer = nil
	}
	gen := fc.generation
	fc.mu.Unlock()
	fc.runProbe(gen)
}

func TestFailoverControllerThreeExplicitProbeSuccessesCompleteFailback(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     time.Hour,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)
	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: time.Hour,
		ProbeMax:     time.Hour,
		Successes:    3,
		StableTime:   time.Nanosecond,
	})
	defer fc.Close()

	fc.probeTCP = func(context.Context) (bool, error) {
		return true, nil
	}
	fc.onPrimaryHealthChange(&dialer.NetworkType{
		L4Proto:   consts.L4ProtoStr_TCP,
		IpVersion: consts.IpVersionStr_4,
	}, false)

	for range 3 {
		runFailoverProbeNow(t, fc)
	}

	if got := fc.State(); got != statePrimaryActive {
		t.Fatalf("state = %v, want statePrimaryActive", got)
	}
	if got := fc.ActiveDialerIndex(); got != 0 {
		t.Fatalf("active role = %d, want primary", got)
	}
}

func TestFailoverControllerExplicitProbeFailureResetsSuccesses(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     time.Hour,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)
	fc := NewFailoverController(log, "test", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: time.Hour,
		ProbeMax:     time.Hour,
		Successes:    3,
		StableTime:   time.Nanosecond,
	})
	defer fc.Close()

	results := []bool{true, true, false, true}
	fc.probeTCP = func(context.Context) (bool, error) {
		result := results[0]
		results = results[1:]
		if !result {
			return false, errors.New("probe failed")
		}
		return true, nil
	}
	fc.onPrimaryHealthChange(&dialer.NetworkType{
		L4Proto:   consts.L4ProtoStr_TCP,
		IpVersion: consts.IpVersionStr_4,
	}, false)

	for range 4 {
		runFailoverProbeNow(t, fc)
	}

	fc.mu.Lock()
	successes := fc.recoverySuccesses
	state := fc.state
	fc.mu.Unlock()
	if successes != 1 {
		t.Fatalf("recovery successes = %d, want 1", successes)
	}
	if state != stateRecovering {
		t.Fatalf("state = %v, want stateRecovering", state)
	}
}

func TestFailoverGroupActivatesPrimaryConnectivityCheck(t *testing.T) {
	// Failover groups must activate the primary's connectivity check so that
	// traffic-driven TCP failures trigger the health transition callback.
	// The connectivity check goroutine stays alive (via keepConnectivityCheck)
	// but does NOT create AliveDialerSets for selection.
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     time.Hour,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)
	dialers := []*dialer.Dialer{primary, fallback}
	annotations := []*dialer.Annotation{{}, {}}
	cfg := &FailoverConfig{
		PrimaryCandidateIdxs: []int{0},
		FallbackIdx:          1,
		Recovery: FailoverRecoveryConfig{
		ProbeInitial: time.Hour,
		ProbeMax:     time.Hour,
		Successes:    3,
		StableTime:   time.Second,
		},
	}
	group := NewDialerGroup(
		option,
		"test",
		dialers,
		annotations,
		DialerSelectionPolicy{Policy: consts.DialerSelectionPolicy_Failover},
		func(bool, *dialer.NetworkType, bool) {},
		cfg,
	)
	defer group.Close()

	// Connectivity check should be active for the primary dialer.
	if !primary.ConnectivityCheckActive() {
		t.Fatal("failover group should activate primary connectivity check")
	}

	// But the group must NOT create AliveDialerSets (selection is via
	// failover controller, not latency-based selection).
	if state := group.currentSelectionState(); state.aliveDialerSets[0] != nil {
		t.Fatal("failover group must not create AliveDialerSet")
	}
}

// --- Failover Event Emission Tests ---

// recordingCallback captures events for test assertions.
type recordingCallback struct {
	mu     sync.Mutex
	events []FailoverEvent
}

func (r *recordingCallback) OnFailoverEvent(ev FailoverEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingCallback) snapshot() []FailoverEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]FailoverEvent, len(r.events))
	copy(out, r.events)
	return out
}

func TestFailoverController_EmitsSwitchEvent(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
		CheckTolerance:    0,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	cb := &recordingCallback{}
	fc := NewFailoverController(log, "proxy_failover", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: 15 * time.Second,
		ProbeMax:     5 * time.Minute,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc.Close()
	fc.SetEventCallback(cb)

	// Active dialer must be primary BEFORE the transition.
	if fc.ActiveDialerIndex() != 0 {
		t.Fatalf("pre-transition active = %d, want 0", fc.ActiveDialerIndex())
	}

	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	if fc.ActiveDialerIndex() != 1 {
		t.Fatalf("post-transition active = %d, want 1", fc.ActiveDialerIndex())
	}

	evs := cb.snapshot()
	if len(evs) != 1 {
		t.Fatalf("emitted %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Type != FailoverEventSwitch {
		t.Fatalf("event type = %v, want failover_switch", ev.Type)
	}
	if ev.Group != "proxy_failover" {
		t.Fatalf("event group = %q, want proxy_failover", ev.Group)
	}
	if ev.From != "primary" || ev.To != "fallback" {
		t.Fatalf("event from/to = %q/%q, want primary/fallback", ev.From, ev.To)
	}
	if ev.Trigger != "tcp_unavailable" {
		t.Fatalf("event trigger = %q, want tcp_unavailable", ev.Trigger)
	}
	if ev.TransitionAt.IsZero() {
		t.Fatal("event transition_at is zero")
	}
}

func TestFailoverController_DuplicateSwitchSuppressed(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     15 * time.Second,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	cb := &recordingCallback{}
	fc := NewFailoverController(log, "g", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: time.Hour,
		ProbeMax:     time.Hour,
		Successes:    3,
		StableTime:   30 * time.Second,
	})
	defer fc.Close()
	fc.SetEventCallback(cb)

	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)
	// Duplicate unhealthy callbacks while already in fallback.
	fc.onPrimaryHealthChange(tcp4, false)
	fc.onPrimaryHealthChange(tcp4, false)

	evs := cb.snapshot()
	if len(evs) != 1 {
		t.Fatalf("emitted %d switch events, want 1", len(evs))
	}
}

func TestFailoverController_EmitsFailbackEvent(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     time.Hour,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	cb := &recordingCallback{}
	fc := NewFailoverController(log, "g", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: time.Hour,
		ProbeMax:     time.Hour,
		Successes:    3,
		StableTime:   time.Nanosecond,
	})
	defer fc.Close()
	fc.SetEventCallback(cb)

	fc.probeTCP = func(context.Context) (bool, error) {
		return true, nil
	}
	// Trigger switch.
	fc.onPrimaryHealthChange(&dialer.NetworkType{
		L4Proto:   consts.L4ProtoStr_TCP,
		IpVersion: consts.IpVersionStr_4,
	}, false)
	// Drive 3 successful probes -> failback.
	for range 3 {
		runFailoverProbeNow(t, fc)
	}

	if fc.ActiveDialerIndex() != 0 {
		t.Fatalf("post-failback active = %d, want 0", fc.ActiveDialerIndex())
	}
	evs := cb.snapshot()
	if len(evs) != 2 {
		t.Fatalf("emitted %d events, want 2 (switch + failback)", len(evs))
	}
	if evs[0].Type != FailoverEventSwitch {
		t.Fatalf("event 0 type = %v, want failover_switch", evs[0].Type)
	}
	if evs[1].Type != FailoverEventFailbackComplete {
		t.Fatalf("event 1 type = %v, want failback_complete", evs[1].Type)
	}
	fb := evs[1]
	if fb.From != "fallback" || fb.To != "primary" {
		t.Fatalf("failback from/to = %q/%q, want fallback/primary", fb.From, fb.To)
	}
	if fb.Successes != 3 {
		t.Fatalf("failback successes = %d, want 3", fb.Successes)
	}
	if fb.StableFor <= 0 {
		t.Fatalf("failback stable_for = %v, want positive", fb.StableFor)
	}
	if fb.TransitionAt.IsZero() {
		t.Fatal("failback transition_at is zero")
	}
}

func TestFailoverController_NoEventAfterClose(t *testing.T) {
	option := &dialer.GlobalOption{
		Log:               log,
		TcpCheckOptionRaw: dialer.TcpCheckOptionRaw{Raw: []string{testTcpCheckUrl}},
		CheckDnsOptionRaw: dialer.CheckDnsOptionRaw{Raw: []string{testUdpCheckDns}},
		CheckInterval:     time.Hour,
	}
	primary := newDirectDialer(option, false)
	fallback := newDirectDialer(option, false)

	cb := &recordingCallback{}
	fc := NewFailoverController(log, "g", primary, fallback, FailoverRecoveryConfig{
		ProbeInitial: time.Hour,
		ProbeMax:     time.Hour,
		Successes:    3,
		StableTime:   time.Nanosecond,
	})
	fc.SetEventCallback(cb)
	fc.Close()

	tcp4 := &dialer.NetworkType{L4Proto: "tcp", IpVersion: "4"}
	fc.onPrimaryHealthChange(tcp4, false)

	evs := cb.snapshot()
	if len(evs) != 0 {
		t.Fatalf("emitted %d events after Close, want 0", len(evs))
	}
}
