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

	// Even when FakeIP is disabled, reject any DNS routing rule that references
	// "fakeip" — such rules would silently fail at lowering time with a confusing
	// error. Catch it early with a clear message.
	if !fakeip.Enabled {
		if err := rejectFakeIPRoutingRefs(params); err != nil {
			return err
		}
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

	// Normalize to the canonical network address (e.g. "198.18.1.5/15" → "198.18.0.0/15")
	// so downstream code sees the same prefix the control plane will use.
	normalized := prefix.Masked()
	if normalized.String() != prefix.String() {
		logrus.Warnf("dns.fakeip.inet4_range: normalized %s to %s (host bits were set)",
			prefix, normalized)
		fakeip.Inet4Range = normalized.String()
	}
	prefix = normalized

	if err := validateFakeIPPrefix(prefix); err != nil {
		return fmt.Errorf("dns.fakeip.inet4_range: %w", err)
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

// validateFakeIPPrefix rejects FakeIP prefixes that overlap with well-known
// reserved address ranges where synthetic DNS responses would cause breakage.
func validateFakeIPPrefix(prefix netip.Prefix) error {
	// RFC 5735: 127.0.0.0/8 — loopback
	if loopback := netip.MustParsePrefix("127.0.0.0/8"); prefix.Overlaps(loopback) {
		return fmt.Errorf("must not overlap with loopback range %s", loopback)
	}
	// RFC 3927: 169.254.0.0/16 — link-local (used for DHCP, mDNS, etc.)
	if lla := netip.MustParsePrefix("169.254.0.0/16"); prefix.Overlaps(lla) {
		return fmt.Errorf("must not overlap with link-local range %s", lla)
	}
	// RFC 1918 private ranges — overlapping would hijack real LAN traffic
	privateRanges := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	for _, r := range privateRanges {
		private := netip.MustParsePrefix(r)
		if prefix.Overlaps(private) {
			return fmt.Errorf("must not overlap with private range %s", private)
		}
	}
	return nil
}

// rejectFakeIPRoutingRefs checks DNS routing rules and fallbacks for references
// to "fakeip" when FakeIP is disabled, returning a clear error on first hit.
func rejectFakeIPRoutingRefs(params *Config) error {
	fakeipName := consts.DnsRequestOutboundIndex_FakeIP.String() // "fakeip"

	checkFunction := func(f *config_parser.Function, where string) error {
		if f == nil {
			return nil
		}
		if f.Name == fakeipName {
			return fmt.Errorf("dns routing %s references %q but dns.fakeip.enabled is false", where, fakeipName)
		}
		return nil
	}

	// DNS request routing: rules + fallback
	for i, rule := range params.Dns.Routing.Request.Rules {
		if err := checkFunction(&rule.Outbound, fmt.Sprintf("request rule[%d]", i)); err != nil {
			return err
		}
	}
	if fb, err := ParseFunctionOrString(params.Dns.Routing.Request.Fallback); err == nil {
		if err := checkFunction(fb, "request fallback"); err != nil {
			return err
		}
	}

	// DNS response routing: rules + fallback
	for i, rule := range params.Dns.Routing.Response.Rules {
		if err := checkFunction(&rule.Outbound, fmt.Sprintf("response rule[%d]", i)); err != nil {
			return err
		}
	}
	if fb, err := ParseFunctionOrString(params.Dns.Routing.Response.Fallback); err == nil {
		if err := checkFunction(fb, "response fallback"); err != nil {
			return err
		}
	}

	return nil
}
