/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"context"
	"testing"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/dns"
	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// e2eMainRule is a tiny convenience to keep the integration test readable.
func e2eMainRule(outbound string, fns ...*config_parser.Function) *config_parser.RoutingRule {
	return &config_parser.RoutingRule{
		AndFunctions: fns,
		Outbound:     config_parser.Function{Name: outbound},
	}
}

func e2eDomainFn(key, value string) *config_parser.Function {
	return &config_parser.Function{Name: "domain", Params: []*config_parser.Param{{Key: key, Val: value}}}
}

func e2eQNameFn(key, value string) *config_parser.Function {
	return &config_parser.Function{Name: "qname", Params: []*config_parser.Param{{Key: key, Val: value}}}
}

func e2eRoutingOutboundRule(names ...string) *config_parser.RoutingRule {
	params := make([]*config_parser.Param, len(names))
	for i, n := range names {
		params[i] = &config_parser.Param{Val: n}
	}
	return &config_parser.RoutingRule{
		AndFunctions: []*config_parser.Function{
			{Name: dns.FunctionRoutingOutbound, Params: params},
		},
		Outbound: config_parser.Function{Name: "fakeip"},
	}
}

// TestRoutingOutbound_Regression_RoutingRulesReleasedBeforeExpand pins the
// invariant that the expansion call MUST happen while the parsed main
// routing rules are still readable. The earlier control-plane wiring set
// `routingA.Rules = nil` BEFORE calling expandFakeIPRoutingOutbound — a
// silent canary failure: result=ok, derived_rules=0, no canary FakeIPs.
// This test simulates that order: take a non-empty rules slice, blank it,
// then call expand. The function MUST fail loudly, NOT return an empty
// expansion with result=ok.
func TestRoutingOutbound_Regression_RoutingRulesReleasedBeforeExpand(t *testing.T) {
	log, hook := newTestLoggerWithHook()

	mainRules := []*config_parser.RoutingRule{
		e2eMainRule("proxy_canary", e2eDomainFn("full", "translate.google.com")),
	}
	dnsRules := []*config_parser.RoutingRule{e2eRoutingOutboundRule("proxy_canary")}

	// Simulate the bug pattern: caller releases the slice before passing it.
	mainRules = nil

	_, err := expandFakeIPRoutingOutbound(log, dnsRules, mainRules,
		func(name string) bool { return name == "proxy_canary" })
	require.Error(t, err, "release-then-expand pattern MUST fail; silent zero-derivation was the canary hazard")

	summary := hook.find("fakeip_auto_expand")
	require.NotNil(t, summary)
	require.Equal(t, "error", summary.Data["result"])
}

//	main routing rules + DNS request rules with routing_outbound(...)
//	  -> expandFakeIPRoutingOutbound (control plane wrapper, with events)
//	    -> dns.New (full DNS controller, including SplitRequestRules and
//	       NewNormalizedRequestRoutingProgram)
//	      -> RequestSelect at runtime
//
// It asserts:
//   - Domains the followed outbound owns route to FakeIP via the derived rule.
//   - Domains it does NOT own fall through to the explicit DNS fallback.
//   - Explicit DNS rules placed BEFORE the routing_outbound(...) selector
//     keep priority over derived rules (ordering precedence honored).
//   - The expansion summary event reflects the actual eligibility decisions.
func TestRoutingOutbound_EndToEnd_ParseExpandMatch(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	log.SetLevel(logrus.DebugLevel)

	mainRoutingRules := []*config_parser.RoutingRule{
		// Pure-domain, follows proxy_canary -> derives.
		e2eMainRule("proxy_canary", e2eDomainFn("full", "translate.google.com")),
		// Pure-domain but different outbound -> SkippedOutbound.
		e2eMainRule("direct", e2eDomainFn("suffix", "cn")),
		// Geosite under domain() -> SkippedGeosite.
		e2eMainRule("proxy_canary", e2eDomainFn("geosite", "cn")),
		// Non-domain function -> SkippedNonDomain.
		e2eMainRule("proxy_canary", &config_parser.Function{
			Name:   "dip",
			Params: []*config_parser.Param{{Val: "8.8.8.8"}},
		}),
		// Pure-domain with multi-suffix tuple, follows proxy_canary -> derives.
		e2eMainRule("proxy_canary", &config_parser.Function{
			Name: "domain",
			Params: []*config_parser.Param{
				{Key: "suffix", Val: "github.com"},
				{Key: "suffix", Val: "githubusercontent.com"},
			},
		}),
	}

	// Explicit DNS rule for gmail BEFORE the selector — ordering precedence
	// test: even if main routing routed gmail.com through proxy_canary, the
	// explicit `qname(suffix: gmail.com) -> alidns` rule must win.
	gmailExplicit := &config_parser.RoutingRule{
		AndFunctions: []*config_parser.Function{e2eQNameFn("suffix", "gmail.com")},
		Outbound:     config_parser.Function{Name: "alidns"},
	}
	dnsRequestRules := []*config_parser.RoutingRule{
		gmailExplicit,
		e2eRoutingOutboundRule("proxy_canary"),
	}

	// Add gmail to main routing too, to prove the explicit DNS rule wins
	// over the derived one even when both could match.
	mainRoutingRules = append(mainRoutingRules,
		e2eMainRule("proxy_canary", e2eDomainFn("suffix", "gmail.com")),
	)

	// Run the control-plane wrapper — same call site control_plane.go uses.
	expanded, err := expandFakeIPRoutingOutbound(log, dnsRequestRules, mainRoutingRules,
		func(name string) bool { return name == "proxy_canary" || name == "direct" })
	require.NoError(t, err)
	// gmailExplicit + 2 derived rules (translate.google.com full + github multi-suffix + gmail.com suffix) = 4
	require.Len(t, expanded, 4, "expected explicit gmail rule + 3 derived rules")

	// Build the actual DNS controller from a config that uses the expanded
	// rules. dns.New runs SplitRequestRules + NewNormalizedRequestRoutingProgram
	// + builds RequestMatcher.
	dnsConf := &config.Dns{
		Upstream: []config.KeyableString{
			"alidns:udp://223.5.5.5:53",
			"foreign:udp://8.8.8.8:53",
		},
		Routing: config.DnsRouting{
			Request: config.DnsRequestRouting{
				Rules:    expanded,
				Fallback: "foreign",
			},
			Response: config.DnsResponseRouting{
				Fallback: "accept",
			},
		},
	}
	s, err := dns.New(dnsConf, &dns.NewOption{
		Logger:                  log,
		UpstreamResolverNetwork: "udp",
	})
	require.NoError(t, err, "dns.New should accept the expanded request rules without further intervention")

	const qtypeA = uint16(1)

	// Case 1: derived domain returns FakeIP.
	idx, _, err := s.RequestSelect(context.Background(), "translate.google.com.", qtypeA)
	require.NoError(t, err)
	require.Equal(t, consts.DnsRequestOutboundIndex_FakeIP, idx,
		"translate.google.com should hit the derived qname(full:) -> fakeip rule")

	// Case 2: multi-suffix derivation hits both suffixes.
	for _, suffixDomain := range []string{"www.github.com.", "raw.githubusercontent.com."} {
		idx, _, err := s.RequestSelect(context.Background(), suffixDomain, qtypeA)
		require.NoError(t, err)
		require.Equal(t, consts.DnsRequestOutboundIndex_FakeIP, idx,
			"%s should hit the derived qname(suffix: github.com, suffix: githubusercontent.com) -> fakeip rule",
			suffixDomain)
	}

	// Case 3: gmail.com is in main routing under proxy_canary AND has an
	// explicit DNS rule above the selector. Ordering must give the explicit
	// rule priority — gmail must NOT return FakeIP, must return alidns.
	idx, _, err = s.RequestSelect(context.Background(), "smtp.gmail.com.", qtypeA)
	require.NoError(t, err)
	require.NotEqual(t, consts.DnsRequestOutboundIndex_FakeIP, idx,
		"explicit qname(suffix: gmail.com) -> alidns must win over the derived rule")

	// Case 4: a domain with no matching rule falls through to the explicit
	// DNS fallback (foreign upstream, NOT fakeip).
	idx, _, err = s.RequestSelect(context.Background(), "example.org.", qtypeA)
	require.NoError(t, err)
	require.NotEqual(t, consts.DnsRequestOutboundIndex_FakeIP, idx,
		"unmatched domain must fall through to the request fallback, not FakeIP")

	// Case 5: domain owned by a NON-followed outbound (direct .cn rule)
	// must not be FakeIP'd — the expander rightly skipped that rule.
	idx, _, err = s.RequestSelect(context.Background(), "www.example.cn.", qtypeA)
	require.NoError(t, err)
	require.NotEqual(t, consts.DnsRequestOutboundIndex_FakeIP, idx,
		"non-followed outbound's domains must not derive a FakeIP rule")

	// Verify the structured summary event reflects what actually happened.
	summary := hook.find("fakeip_auto_expand")
	require.NotNil(t, summary)
	require.Equal(t, "ok", summary.Data["result"])
	require.Equal(t, "routing_outbound", summary.Data["selector"])
	require.Equal(t, []string{"proxy_canary"}, summary.Data["follow_outbounds"])
	// translate.google.com (full) + github multi-suffix (1 rule, both suffixes
	// in one Param group) + gmail.com (suffix) = 3 derived rules.
	require.Equal(t, 3, summary.Data["derived_rules"])
	require.Equal(t, 1, summary.Data["skipped_outbound"], "the .cn direct rule should be counted as skipped_outbound")
	require.Equal(t, 1, summary.Data["skipped_geosite"], "the geosite:cn rule should be counted as skipped_geosite")
	require.Equal(t, 1, summary.Data["skipped_non_domain"], "the dip(8.8.8.8) rule should be counted as skipped_non_domain")
	require.Equal(t, 0, summary.Data["skipped_compound"])

	// At debug level, every derived domain must produce its own event.
	derived := hook.findAll("fakeip_auto_derived")
	require.Len(t, derived, 4, "translate.google.com + github.com + githubusercontent.com + gmail.com = 4 derived domains")
}
