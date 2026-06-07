/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package dialer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
)

func configureLatencyProbeDialer(t *testing.T, d *Dialer, serverURL string) {
	t.Helper()

	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", serverURL, err)
	}
	d.TcpCheckOptionRaw.Reset()
	d.TcpCheckOptionRaw.Raw = []string{serverURL, u.Hostname()}
	d.TcpCheckOptionRaw.Method = http.MethodGet
}

func TestDialerProbeLatencyFastDoesNotMutateHealthState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	d := newTestDialer(t)
	configureLatencyProbeDialer(t, d, server.URL)

	result, err := d.ProbeLatencyFast()
	if err != nil {
		t.Fatalf("ProbeLatencyFast() error = %v", err)
	}
	if result == nil {
		t.Fatal("ProbeLatencyFast() returned nil result")
	}
	if !result.Alive {
		t.Fatalf("ProbeLatencyFast() alive = false, message = %q", result.Message)
	}
	if result.Latency <= 0 {
		t.Fatalf("ProbeLatencyFast() latency = %v, want > 0", result.Latency)
	}
	if result.CheckedAt.IsZero() {
		t.Fatal("ProbeLatencyFast() should stamp CheckedAt")
	}

	networkType := &NetworkType{
		L4Proto:   consts.L4ProtoStr_TCP,
		IpVersion: consts.IpVersionStr_4,
	}
	if got := d.MustGetLatencies10(networkType).Len(); got != 0 {
		t.Fatalf("fast probe should not append latency samples, got %d", got)
	}
}

func TestDialerProbeLatencyRecordsBestLatency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	d := newTestDialer(t)
	configureLatencyProbeDialer(t, d, server.URL)

	result, err := d.ProbeLatency()
	if err != nil {
		t.Fatalf("ProbeLatency() error = %v", err)
	}
	if result == nil {
		t.Fatal("ProbeLatency() returned nil result")
	}
	if !result.Alive {
		t.Fatalf("ProbeLatency() alive = false, message = %q", result.Message)
	}
	if result.Latency <= 0 {
		t.Fatalf("ProbeLatency() latency = %v, want > 0", result.Latency)
	}

	networkType := &NetworkType{
		L4Proto:   consts.L4ProtoStr_TCP,
		IpVersion: consts.IpVersionStr_4,
	}
	lastLatency, ok := d.MustGetLatencies10(networkType).LastLatency()
	if !ok {
		t.Fatal("ProbeLatency() should append a latency sample")
	}
	if result.Latency != lastLatency {
		t.Fatalf("ProbeLatency() latency = %v, want last sample %v", result.Latency, lastLatency)
	}
}

func TestProbeTCPOnceReturnsFreshSuccessForEveryInvocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	d := newTestDialer(t)
	configureLatencyProbeDialer(t, d, server.URL)

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ok, err := d.ProbeTCPOnce(ctx)
		cancel()
		if err != nil {
			t.Fatalf("probe %d returned error: %v", i+1, err)
		}
		if !ok {
			t.Fatalf("probe %d returned false, want true", i+1)
		}
	}
}

func TestProbeTCPOnceRestoresCanonicalTCPHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	d := newTestDialer(t)
	configureLatencyProbeDialer(t, d, server.URL)
	networkType := &NetworkType{
		L4Proto:   consts.L4ProtoStr_TCP,
		IpVersion: consts.IpVersionStr_4,
	}
	d.informDialerGroupUpdate(d.markUnavailableInternal(networkType, true, false))
	if d.MustGetAlive(networkType) {
		t.Fatal("test setup failed: TCP/IPv4 should be unavailable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	ok, err := d.ProbeTCPOnce(ctx)
	cancel()
	if err != nil {
		t.Fatalf("ProbeTCPOnce() error = %v", err)
	}
	if !ok {
		t.Fatal("ProbeTCPOnce() returned false, want true")
	}
	if !d.MustGetAlive(networkType) {
		t.Fatal("successful one-shot probe did not restore canonical TCP/IPv4 health")
	}
}

func TestProbeTCPOnceHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	d := newTestDialer(t)
	configureLatencyProbeDialer(t, d, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		ok, err := d.ProbeTCPOnce(ctx)
		if ok {
			result <- errors.New("cancelled probe returned success")
			return
		}
		result <- err
	}()

	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ProbeTCPOnce did not return after cancellation")
	}
}

func TestProbeTCPOptionsOnceReturnsWhenEitherFamilySucceeds(t *testing.T) {
	blocked := make(chan struct{})
	options := []*CheckOption{
		{
			networkType: &NetworkType{
				L4Proto:   consts.L4ProtoStr_TCP,
				IpVersion: consts.IpVersionStr_4,
			},
			CheckFunc: func(ctx context.Context, _ *NetworkType) (bool, error) {
				close(blocked)
				<-ctx.Done()
				return false, ctx.Err()
			},
		},
		{
			networkType: &NetworkType{
				L4Proto:   consts.L4ProtoStr_TCP,
				IpVersion: consts.IpVersionStr_6,
			},
			CheckFunc: func(context.Context, *NetworkType) (bool, error) {
				<-blocked
				return true, nil
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	ok, err := probeTCPOptionsOnce(ctx, options)
	if err != nil {
		t.Fatalf("probeTCPOptionsOnce() error = %v", err)
	}
	if !ok {
		t.Fatal("probeTCPOptionsOnce() returned false, want true")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("probe waited for blocked family: %v", elapsed)
	}
}
