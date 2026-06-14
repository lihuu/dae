/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package config

import (
	"fmt"
	"net/netip"

	"github.com/daeuniverse/dae/common"
	"github.com/sirupsen/logrus"
	"strings"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/pkg/config_parser"
)

type patch func(params *Config) error

var patches = []patch{
	patchBootstrapResolver,
	patchTcpCheckHttpMethod,
	patchEmptyDns,
	patchMustOutbound,
	patchDnsFakeIP,
}

func patchBootstrapResolver(params *Config) error {
	_, err := BootstrapResolvers(&params.Global)
	return err
}

func patchTcpCheckHttpMethod(params *Config) error {
	if !common.IsValidHttpMethod(params.Global.TcpCheckHttpMethod) {
		logrus.Warnf("Unknown HTTP Method '%v'. Fallback to 'CONNECT'.", params.Global.TcpCheckHttpMethod)
		params.Global.TcpCheckHttpMethod = "CONNECT"
	}
	return nil
}

func patchEmptyDns(params *Config) error {
	if params.Dns.Routing.Request.Fallback == nil {
		params.Dns.Routing.Request.Fallback = consts.DnsRequestOutboundIndex_AsIs.String()
	}
	if params.Dns.Routing.Response.Fallback == nil {
		params.Dns.Routing.Response.Fallback = consts.DnsResponseOutboundIndex_Accept.String()
	}
	return nil
}

func patchMustOutbound(params *Config) error {
	for i := range params.Routing.Rules {
		if strings.HasPrefix(params.Routing.Rules[i].Outbound.Name, "must_") {
			if params.Routing.Rules[i].Outbound.Name == "must_rules" {
				// Reserve must_rules.
				continue
			}
			params.Routing.Rules[i].Outbound.Name = strings.TrimPrefix(params.Routing.Rules[i].Outbound.Name, "must_")
			params.Routing.Rules[i].Outbound.Params = append(params.Routing.Rules[i].Outbound.Params, &config_parser.Param{
				Val: "must",
			})
		}
	}
	f, err := ParseFunctionOrString(params.Routing.Fallback)
	if err != nil {
		return err
	}
	if strings.HasPrefix(f.Name, "must_") {
		f.Name = strings.TrimPrefix(f.Name, "must_")
		f.Params = append(f.Params, &config_parser.Param{
			Val: "must",
		})
		params.Routing.Fallback = f
	}
	return nil
}

func patchDnsFakeIP(params *Config) error {
	fakeip := &params.Dns.FakeIP
	if !fakeip.Enabled {
		return nil
	}

	prefix, err := netip.ParsePrefix(fakeip.Inet4Range)
	if err != nil {
		return fmt.Errorf("dns.fakeip.inet4_range: %w", err)
	}
	if !prefix.Addr().Is4() {
		return fmt.Errorf("dns.fakeip.inet4_range: must be an IPv4 prefix")
	}
	if prefix.Bits() > 30 {
		return fmt.Errorf("dns.fakeip.inet4_range: /%d has no allocatable IPv4 addresses", prefix.Bits())
	}

	if fakeip.TTL <= 0 {
		return fmt.Errorf("dns.fakeip.ttl: ttl must be positive")
	}

	if fakeip.Store == "" {
		return fmt.Errorf("dns.fakeip.store: store path must not be empty")
	}

	if fakeip.DirectUpstream == "" {
		return fmt.Errorf("dns.fakeip.direct_upstream is required when fakeip is enabled")
	}

	switch strings.ToLower(fakeip.DirectUpstream) {
	case "fakeip", "asis", "reject":
		return fmt.Errorf("dns.fakeip.direct_upstream must be a real DNS upstream, not %q", fakeip.DirectUpstream)
	}

	upstreamNames := make(map[string]bool)
	for _, upstreamRaw := range params.Dns.Upstream {
		tag, _ := common.GetTagFromLinkLikePlaintext(string(upstreamRaw))
		if tag != "" {
			upstreamNames[tag] = true
		}
	}
	if !upstreamNames[fakeip.DirectUpstream] {
		return fmt.Errorf("dns.fakeip.direct_upstream: upstream %q not found in dns.upstream", fakeip.DirectUpstream)
	}

	return nil
}
