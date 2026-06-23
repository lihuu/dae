/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package dialer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/outbound/netproxy"
)

// LatencyProbeResult reports the latest ad-hoc latency probe result for a dialer.
type LatencyProbeResult struct {
	Alive     bool
	Latency   time.Duration
	Message   string
	CheckedAt time.Time
}

const fastLatencyProbeTimeout = 1500 * time.Millisecond

// ProbeTCPOnce runs one cancellable TCP connectivity probe and updates the
// canonical TCP health state. Each invocation still returns its own result,
// even when that health state does not transition.
func (d *Dialer) ProbeTCPOnce(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, fmt.Errorf("nil TCP probe context")
	}

	parsed, err := ParseTcpCheckOption(
		ctx,
		d.TcpCheckOptionRaw.Raw,
		d.TcpCheckOptionRaw.Method,
		d.TcpCheckOptionRaw.ResolverNetwork,
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("failed to parse tcp_check_url: %w", err)
	}

	var tcpSoMark uint32
	var mptcp bool
	if network, err := netproxy.ParseMagicNetwork(d.TcpCheckOptionRaw.ResolverNetwork); err == nil {
		tcpSoMark = network.Mark
		mptcp = network.Mptcp
	}
	options := []*CheckOption{
		{
			networkType: &NetworkType{
				L4Proto:   consts.L4ProtoStr_TCP,
				IpVersion: consts.IpVersionStr_4,
			},
			CheckFunc: func(ctx context.Context, _ *NetworkType) (bool, error) {
				if !parsed.Ip4.IsValid() {
					return false, nil
				}
				return d.runObservedTCPProbe(
					ctx,
					&NetworkType{
						L4Proto:   consts.L4ProtoStr_TCP,
						IpVersion: consts.IpVersionStr_4,
					},
					func(ctx context.Context) (bool, error) {
						return d.HttpCheck(ctx, IdxTcp4, parsed.Url, parsed.Ip4, parsed.Method, tcpSoMark, mptcp)
					},
				)
			},
		},
		{
			networkType: &NetworkType{
				L4Proto:   consts.L4ProtoStr_TCP,
				IpVersion: consts.IpVersionStr_6,
			},
			CheckFunc: func(ctx context.Context, _ *NetworkType) (bool, error) {
				if !parsed.Ip6.IsValid() {
					return false, nil
				}
				return d.runObservedTCPProbe(
					ctx,
					&NetworkType{
						L4Proto:   consts.L4ProtoStr_TCP,
						IpVersion: consts.IpVersionStr_6,
					},
					func(ctx context.Context) (bool, error) {
						return d.HttpCheck(ctx, IdxTcp6, parsed.Url, parsed.Ip6, parsed.Method, tcpSoMark, mptcp)
					},
				)
			},
		},
	}
	return probeTCPOptionsOnce(ctx, options)
}

func (d *Dialer) runObservedTCPProbe(
	ctx context.Context,
	networkType *NetworkType,
	probe func(context.Context) (bool, error),
) (bool, error) {
	checkedAt := time.Now()
	start := checkedAt
	ok, err := probe(ctx)
	latency := time.Since(start)

	if ok && err == nil {
		d.collectionFineMu.Lock()
		collection := d.mustGetCollection(networkType)
		collection.LastProbe = DialerProbeObservationSnapshot{
			CheckedAt:  checkedAt,
			Alive:      true,
			Latency:    latency,
			HasLatency: true,
			Message:    FormatLatencyMessage(&LatencyProbeResult{Alive: true, Latency: latency}),
		}
		d.collectionFineMu.Unlock()

		update, _ := d.markAvailable(networkType, latency)
		d.informDialerGroupUpdate(update)
	} else if err != nil && !errors.Is(err, context.Canceled) {
		d.collectionFineMu.Lock()
		collection := d.mustGetCollection(networkType)
		collection.LastProbe = DialerProbeObservationSnapshot{
			CheckedAt: checkedAt,
			Alive:     false,
			Message:   err.Error(),
		}
		d.collectionFineMu.Unlock()

		d.logUnavailable(networkType, err)
		d.informDialerGroupUpdate(d.markUnavailable(networkType))
	}
	return ok, err
}

func probeTCPOptionsOnce(ctx context.Context, options []*CheckOption) (bool, error) {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type probeResult struct {
		ok  bool
		err error
	}
	results := make(chan probeResult, len(options))
	for _, option := range options {
		option := option
		go func() {
			ok, err := option.CheckFunc(probeCtx, option.networkType)
			results <- probeResult{ok: ok, err: err}
		}()
	}

	var lastErr error
	for range options {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case result := <-results:
			if result.ok && result.err == nil {
				return true, nil
			}
			if result.err != nil {
				lastErr = result.err
			}
		}
	}

	if lastErr != nil {
		return false, lastErr
	}
	return false, fmt.Errorf("TCP probe has no reachable address family")
}

// ProbeLatency runs the normal TCP health-check path, mutates the dialer's
// health state/history, and returns the best recorded latency across
// supported IP families.
func (d *Dialer) ProbeLatency() (*LatencyProbeResult, error) {
	checkOptions := d.latencyProbeCheckOptions()
	var (
		bestLatency time.Duration
		hasLatency  bool
		lastErr     error
	)

	for _, opt := range checkOptions {
		ok, err := d.Check(opt)
		if err != nil {
			lastErr = err
		}
		if !ok {
			continue
		}

		latency, hasLastLatency := d.MustGetLatencies10(opt.networkType).LastLatency()
		if !hasLastLatency {
			continue
		}
		if !hasLatency || latency < bestLatency {
			bestLatency = latency
			hasLatency = true
		}
	}

	result := &LatencyProbeResult{
		Alive:     hasLatency,
		CheckedAt: time.Now(),
	}
	if hasLatency {
		result.Latency = bestLatency
		return result, nil
	}

	if lastErr != nil {
		result.Message = lastErr.Error()
		return result, nil
	}

	result.Message = "no latency result"
	return result, nil
}

// ProbeLatencyFast runs a bounded TCP probe without mutating dialer health
// state and returns the best measured latency across supported IP families.
func (d *Dialer) ProbeLatencyFast() (*LatencyProbeResult, error) {
	checkOptions := d.latencyProbeCheckOptions()
	var (
		bestLatency time.Duration
		hasLatency  bool
		lastErr     error
	)

	probeParent := context.Background()
	if d != nil && d.ctx != nil {
		probeParent = d.ctx
	}

	for _, opt := range checkOptions {
		ctx, cancel := context.WithTimeout(probeParent, fastLatencyProbeTimeout)
		start := time.Now()
		ok, err := opt.CheckFunc(ctx, opt.networkType)
		latency := time.Since(start)
		cancel()

		if err != nil {
			lastErr = err
		}
		if !ok || err != nil {
			continue
		}

		if !hasLatency || latency < bestLatency {
			bestLatency = latency
			hasLatency = true
		}
	}

	result := &LatencyProbeResult{
		Alive:     hasLatency,
		CheckedAt: time.Now(),
	}
	if hasLatency {
		result.Latency = bestLatency
		return result, nil
	}

	if lastErr != nil {
		result.Message = lastErr.Error()
		return result, nil
	}

	result.Message = "no latency result"
	return result, nil
}

func (d *Dialer) latencyProbeCheckOptions() []*CheckOption {
	return []*CheckOption{
		{
			networkType: &NetworkType{
				L4Proto:   consts.L4ProtoStr_TCP,
				IpVersion: consts.IpVersionStr_4,
			},
			CheckFunc: func(ctx context.Context, _ *NetworkType) (bool, error) {
				opt, err := d.TcpCheckOptionRaw.Option()
				if err != nil {
					return false, err
				}
				if !opt.Ip4.IsValid() {
					return false, nil
				}
				var tcpSoMark uint32
				var mptcp bool
				if network, err := netproxy.ParseMagicNetwork(d.TcpCheckOptionRaw.ResolverNetwork); err == nil {
					tcpSoMark = network.Mark
					mptcp = network.Mptcp
				}
				return d.HttpCheck(ctx, IdxTcp4, opt.Url, opt.Ip4, opt.Method, tcpSoMark, mptcp)
			},
		},
		{
			networkType: &NetworkType{
				L4Proto:   consts.L4ProtoStr_TCP,
				IpVersion: consts.IpVersionStr_6,
			},
			CheckFunc: func(ctx context.Context, _ *NetworkType) (bool, error) {
				opt, err := d.TcpCheckOptionRaw.Option()
				if err != nil {
					return false, err
				}
				if !opt.Ip6.IsValid() {
					return false, nil
				}
				var tcpSoMark uint32
				var mptcp bool
				if network, err := netproxy.ParseMagicNetwork(d.TcpCheckOptionRaw.ResolverNetwork); err == nil {
					tcpSoMark = network.Mark
					mptcp = network.Mptcp
				}
				return d.HttpCheck(ctx, IdxTcp6, opt.Url, opt.Ip6, opt.Method, tcpSoMark, mptcp)
			},
		},
	}
}

// FormatLatencyMessage formats a latency probe result for status display.
func FormatLatencyMessage(result *LatencyProbeResult) string {
	if result == nil {
		return "unknown"
	}
	if result.Alive {
		return fmt.Sprintf("%dms", result.Latency.Milliseconds())
	}
	if result.Message != "" {
		return result.Message
	}
	return "unavailable"
}
