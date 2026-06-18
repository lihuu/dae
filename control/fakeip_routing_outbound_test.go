/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"errors"
	"testing"

	"github.com/daeuniverse/dae/component/dns"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func mainDomainRule(outbound, key, value string) *config_parser.RoutingRule {
	return &config_parser.RoutingRule{
		AndFunctions: []*config_parser.Function{{
			Name:   "domain",
			Params: []*config_parser.Param{{Key: key, Val: value}},
		}},
		Outbound: config_parser.Function{Name: outbound},
	}
}

func mainDipRule(outbound, value string) *config_parser.RoutingRule {
	return &config_parser.RoutingRule{
		AndFunctions: []*config_parser.Function{{
			Name:   "dip",
			Params: []*config_parser.Param{{Val: value}},
		}},
		Outbound: config_parser.Function{Name: outbound},
	}
}

func dnsRoutingOutboundRule(outbounds ...string) *config_parser.RoutingRule {
	params := make([]*config_parser.Param, len(outbounds))
	for i, o := range outbounds {
		params[i] = &config_parser.Param{Val: o}
	}
	return &config_parser.RoutingRule{
		AndFunctions: []*config_parser.Function{
			{Name: dns.FunctionRoutingOutbound, Params: params},
		},
		Outbound: config_parser.Function{Name: "fakeip"},
	}
}

func TestExpandFakeIPRoutingOutbound_NoSelectorEmitsNoEvents(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	dnsRules := []*config_parser.RoutingRule{
		// A normal qname rule — must pass through, no expansion event.
		{
			AndFunctions: []*config_parser.Function{{Name: "qname", Params: []*config_parser.Param{{Key: "suffix", Val: "cn"}}}},
			Outbound:     config_parser.Function{Name: "alidns"},
		},
	}
	out, err := expandFakeIPRoutingOutbound(log, dnsRules, nil, func(string) bool { return false })
	require.NoError(t, err)
	require.Equal(t, dnsRules, out)
	require.Nil(t, hook.find("fakeip_auto_expand"))
	require.Nil(t, hook.find("fakeip_auto_derived"))
}

func TestExpandFakeIPRoutingOutbound_EmitsSummaryAndDerivedEvents(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	log.SetLevel(logrus.TraceLevel)

	mainRules := []*config_parser.RoutingRule{
		mainDomainRule("proxy_fakeip_auto_canary", "full", "translate.google.com"),
		mainDomainRule("direct", "suffix", "cn"),
		mainDipRule("proxy_fakeip_auto_canary", "8.8.8.8"),
		mainDomainRule("proxy_fakeip_auto_canary", "geosite", "cn"),
	}
	dnsRules := []*config_parser.RoutingRule{dnsRoutingOutboundRule("proxy_fakeip_auto_canary")}
	exists := func(n string) bool { return n == "proxy_fakeip_auto_canary" || n == "direct" }

	out, err := expandFakeIPRoutingOutbound(log, dnsRules, mainRules, exists)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, "fakeip", out[0].Outbound.Name)
	require.Equal(t, "qname", out[0].AndFunctions[0].Name)

	summary := hook.find("fakeip_auto_expand")
	require.NotNil(t, summary, "fakeip_auto_expand summary should be emitted")
	require.Equal(t, "fakeip", summary.Data["component"])
	require.Equal(t, "routing_outbound", summary.Data["selector"])
	require.Equal(t, "ok", summary.Data["result"])
	require.Equal(t, []string{"proxy_fakeip_auto_canary"}, summary.Data["follow_outbounds"])
	require.Equal(t, 1, summary.Data["derived_rules"])
	require.Equal(t, 1, summary.Data["skipped_non_domain"])
	require.Equal(t, 1, summary.Data["skipped_geosite"])
	require.Equal(t, 0, summary.Data["skipped_compound"])
	require.Equal(t, 1, summary.Data["skipped_outbound"])

	derived := hook.findAll("fakeip_auto_derived")
	require.Len(t, derived, 1)
	require.Equal(t, "full", derived[0].Data["domain_key"])
	require.Equal(t, "translate.google.com", derived[0].Data["domain"])
	require.Equal(t, "proxy_fakeip_auto_canary", derived[0].Data["routing_outbound"])
	require.Equal(t, "fakeip", derived[0].Data["dns_outbound"])
	require.Equal(t, 0, derived[0].Data["source_rule_index"])
}

func TestExpandFakeIPRoutingOutbound_RejectsNonFakeIPOutboundWithErrorEvent(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	bad := dnsRoutingOutboundRule("proxy")
	bad.Outbound.Name = "alidns"
	_, err := expandFakeIPRoutingOutbound(log, []*config_parser.RoutingRule{bad}, nil, func(string) bool { return true })
	require.Error(t, err)
	require.True(t, errors.Is(err, dns.ErrRoutingOutboundOnlyFakeIP))

	summary := hook.find("fakeip_auto_expand")
	require.NotNil(t, summary, "summary event should be emitted even on validation failure")
	require.Equal(t, "error", summary.Data["result"])
}

func TestExpandFakeIPRoutingOutbound_DerivedDebugSuppressedBelowDebugLevel(t *testing.T) {
	// At Info level, fakeip_auto_derived (Debug) must NOT fire even though
	// the summary (Info) does. This protects production-default log
	// volume — derived events must be opt-in.
	log, hook := newTestLoggerWithHook()
	log.SetLevel(logrus.InfoLevel)
	mainRules := []*config_parser.RoutingRule{
		mainDomainRule("p", "suffix", "example.com"),
	}
	dnsRules := []*config_parser.RoutingRule{dnsRoutingOutboundRule("p")}
	_, err := expandFakeIPRoutingOutbound(log, dnsRules, mainRules, func(n string) bool { return n == "p" })
	require.NoError(t, err)
	require.NotNil(t, hook.find("fakeip_auto_expand"))
	require.Empty(t, hook.findAll("fakeip_auto_derived"))
}

// Regression: expansion must FAIL LOUDLY when the selector is present but
// the main routing rules slice is nil/empty. The bug we are guarding
// against: the control plane previously called this AFTER a
// `routingA.Rules = nil` release, silently producing
// `result=ok derived_rules=0` and shipping with the canary domains never
// becoming FakeIP. A typed error is the only safe behavior — the alternative
// (silent no-op) was the production hazard.
func TestExpandFakeIPRoutingOutbound_RejectsEmptyMainRoutingRulesWithSelectorPresent(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	dnsRules := []*config_parser.RoutingRule{dnsRoutingOutboundRule("proxy_canary")}
	_, err := expandFakeIPRoutingOutbound(log, dnsRules, nil, func(n string) bool { return n == "proxy_canary" })
	require.Error(t, err, "selector with empty main routing rules MUST fail loudly, not silently expand to nothing")

	summary := hook.find("fakeip_auto_expand")
	require.NotNil(t, summary, "summary event MUST fire on the failure path so operators see the typed error")
	require.Equal(t, "error", summary.Data["result"])
}

// And the other way around: nil/empty main routing AND no selector is fine.
// We must not break callers that have neither.
func TestExpandFakeIPRoutingOutbound_AllowsEmptyMainRoutingWhenNoSelectorPresent(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	dnsRules := []*config_parser.RoutingRule{
		{
			AndFunctions: []*config_parser.Function{{Name: "qname", Params: []*config_parser.Param{{Key: "suffix", Val: "cn"}}}},
			Outbound:     config_parser.Function{Name: "alidns"},
		},
	}
	out, err := expandFakeIPRoutingOutbound(log, dnsRules, nil, func(string) bool { return true })
	require.NoError(t, err)
	require.Equal(t, dnsRules, out)
	require.Nil(t, hook.find("fakeip_auto_expand"))
}
