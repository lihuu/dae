// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>

package config

import (
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

func newMinimalConfig() *Config {
	return &Config{
		Dns: Dns{
			Upstream: []KeyableString{"myupstream:udp://8.8.8.8:53"},
			Routing: DnsRouting{
				Request: DnsRequestRouting{
					Fallback: "asis",
				},
				Response: DnsResponseRouting{
					Fallback: "accept",
				},
			},
			FakeIP: DnsFakeIP{
				Enabled:        true,
				Inet4Range:     "198.18.0.0/15",
				TTL:            60,
				Store:          "/tmp/fakeip.db",
				DirectUpstream: "myupstream",
			},
		},
	}
}

func TestPatchDnsFakeIP_ValidConfig(t *testing.T) {
	params := newMinimalConfig()
	require.NoError(t, patchDnsFakeIP(params))
}

func TestPatchDnsFakeIP_NormalizesPrefix(t *testing.T) {
	params := newMinimalConfig()
	// Host bits set — should be normalized to network address.
	params.Dns.FakeIP.Inet4Range = "198.18.1.5/15"
	require.NoError(t, patchDnsFakeIP(params))
	require.Equal(t, "198.18.0.0/15", params.Dns.FakeIP.Inet4Range,
		"prefix should be normalized to canonical network address")
}

func TestPatchDnsFakeIP_RejectsLoopbackOverlap(t *testing.T) {
	params := newMinimalConfig()
	params.Dns.FakeIP.Inet4Range = "127.0.0.0/8"
	err := patchDnsFakeIP(params)
	require.Error(t, err)
	require.Contains(t, err.Error(), "loopback")
}

func TestPatchDnsFakeIP_RejectsLinkLocalOverlap(t *testing.T) {
	params := newMinimalConfig()
	params.Dns.FakeIP.Inet4Range = "169.254.0.0/16"
	err := patchDnsFakeIP(params)
	require.Error(t, err)
	require.Contains(t, err.Error(), "link-local")
}

func TestPatchDnsFakeIP_RejectsPrivateRangeOverlap(t *testing.T) {
	for _, r := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		t.Run(r, func(t *testing.T) {
			params := newMinimalConfig()
			params.Dns.FakeIP.Inet4Range = r
			err := patchDnsFakeIP(params)
			require.Error(t, err)
			require.Contains(t, err.Error(), "private")
		})
	}
}

func TestPatchDnsFakeIP_DisabledRejectsFakeIPRoutingRef(t *testing.T) {
	params := newMinimalConfig()
	params.Dns.FakeIP.Enabled = false

	// A DNS request routing rule referencing "fakeip" should be rejected.
	params.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		{
			Outbound: config_parser.Function{Name: "fakeip"},
		},
	}
	err := patchDnsFakeIP(params)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fakeip")
	require.Contains(t, err.Error(), "false")
}

func TestPatchDnsFakeIP_DisabledRejectsFakeIPFallback(t *testing.T) {
	params := newMinimalConfig()
	params.Dns.FakeIP.Enabled = false
	params.Dns.Routing.Request.Fallback = "fakeip"

	err := patchDnsFakeIP(params)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fakeip")
	require.Contains(t, err.Error(), "false")
}

func TestPatchDnsFakeIP_DisabledAcceptsNonFakeIPRouting(t *testing.T) {
	params := newMinimalConfig()
	params.Dns.FakeIP.Enabled = false

	params.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		{
			Outbound: config_parser.Function{Name: "asis"},
		},
	}
	require.NoError(t, patchDnsFakeIP(params))
}
