/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package dialer

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/daeuniverse/outbound/netproxy"
	"github.com/sirupsen/logrus"
)

type proxyTransportDiagnosticContext struct {
	trafficTarget string
	sniffedDomain string
	outbound      string
	policy        string
}

type proxyTransportDiagnosticContextKey struct{}

// WithProxyTransportDiagnosticContext attaches the routed destination to the
// transport dial so its actual upstream address can be correlated in logs.
func WithProxyTransportDiagnosticContext(
	ctx context.Context,
	trafficTarget string,
	sniffedDomain string,
	outbound string,
	policy string,
) context.Context {
	return context.WithValue(ctx, proxyTransportDiagnosticContextKey{}, proxyTransportDiagnosticContext{
		trafficTarget: trafficTarget,
		sniffedDomain: sniffedDomain,
		outbound:      outbound,
		policy:        policy,
	})
}

type proxyTransportDiagnosticDialer struct {
	netproxy.Dialer
	log       *logrus.Logger
	node      string
	proxyAddr string
}

func newProxyTransportDiagnosticDialer(
	parent netproxy.Dialer,
	log *logrus.Logger,
	node string,
	proxyAddr string,
) netproxy.Dialer {
	if parent == nil || log == nil || proxyAddr == "" {
		return parent
	}
	return &proxyTransportDiagnosticDialer{
		Dialer:    parent,
		log:       log,
		node:      node,
		proxyAddr: proxyAddr,
	}
}

func (d *proxyTransportDiagnosticDialer) DialContext(ctx context.Context, network, addr string) (netproxy.Conn, error) {
	startedAt := time.Now()
	conn, err := d.Dialer.DialContext(ctx, network, addr)
	metadata, hasMetadata := ctx.Value(proxyTransportDiagnosticContextKey{}).(proxyTransportDiagnosticContext)
	if err == nil && (!hasMetadata || metadata.policy != "failover") {
		return conn, nil
	}

	fields := logrus.Fields{
		"event":           "proxy_transport_dial",
		"node":            d.node,
		"proxy_addr":      d.proxyAddr,
		"actual_addr":     addr,
		"network":         readableDiagnosticNetwork(network),
		"duration_ms":     time.Since(startedAt).Milliseconds(),
		"dialer_instance": fmt.Sprintf("%p", d),
		"purpose":         "internal",
	}
	if hasMetadata {
		fields["traffic_target"] = metadata.trafficTarget
		fields["sniffed_domain"] = metadata.sniffedDomain
		fields["outbound"] = metadata.outbound
		fields["policy"] = metadata.policy
		fields["purpose"] = "traffic"
	}

	entry := d.log.WithFields(fields)
	if err != nil {
		entry.WithError(err).WithField("result", "failure").Warn("Proxy transport dial failed")
		return nil, err
	}
	entry.WithField("result", "success").Info("Proxy transport dial succeeded")
	return conn, nil
}

func readableDiagnosticNetwork(network string) string {
	magicNetwork, err := netproxy.ParseMagicNetwork(network)
	if err != nil {
		return network
	}
	if magicNetwork.IPVersion != "" {
		return magicNetwork.Network + magicNetwork.IPVersion
	}
	return magicNetwork.Network
}

func (d *proxyTransportDiagnosticDialer) LookupIPAddr(ctx context.Context, network, host string) ([]net.IPAddr, error) {
	resolver, ok := d.Dialer.(lookupIPDialer)
	if !ok {
		return net.DefaultResolver.LookupIPAddr(ctx, host)
	}
	return resolver.LookupIPAddr(ctx, network, host)
}
