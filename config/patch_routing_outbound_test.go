/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package config

import (
	"strings"
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
)

func routingOutboundDnsRule(outbound string, names ...string) *config_parser.RoutingRule {
	params := make([]*config_parser.Param, len(names))
	for i, n := range names {
		params[i] = &config_parser.Param{Val: n}
	}
	return &config_parser.RoutingRule{
		AndFunctions: []*config_parser.Function{
			{Name: "routing_outbound", Params: params},
		},
		Outbound: config_parser.Function{Name: outbound},
	}
}

func mainRoutingOutboundRule(outbound string, names ...string) *config_parser.RoutingRule {
	// Same shape but used in a non-DNS context.
	return routingOutboundDnsRule(outbound, names...)
}

func TestPatchRoutingOutbound_AcceptsValidDnsRequestRule(t *testing.T) {
	cfg := &Config{}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		routingOutboundDnsRule("fakeip", "proxy_canary"),
	}
	if err := patchRoutingOutbound(cfg); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestPatchRoutingOutbound_RejectsNonFakeIPTarget(t *testing.T) {
	cfg := &Config{}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		routingOutboundDnsRule("alidns", "proxy_canary"),
	}
	err := patchRoutingOutbound(cfg)
	if err == nil {
		t.Fatalf("expected error for non-fakeip target, got nil")
	}
	if !strings.Contains(err.Error(), "fakeip") {
		t.Fatalf("error %q should mention fakeip", err.Error())
	}
}

func TestPatchRoutingOutbound_RejectsEmptyArgList(t *testing.T) {
	cfg := &Config{}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		routingOutboundDnsRule("fakeip"),
	}
	if err := patchRoutingOutbound(cfg); err == nil {
		t.Fatalf("expected error for empty arg list, got nil")
	}
}

func TestPatchRoutingOutbound_RejectsUseOutsideDnsRequest(t *testing.T) {
	t.Run("dns response", func(t *testing.T) {
		cfg := &Config{}
		cfg.Dns.Routing.Response.Rules = []*config_parser.RoutingRule{
			routingOutboundDnsRule("fakeip", "p"),
		}
		err := patchRoutingOutbound(cfg)
		if err == nil {
			t.Fatalf("expected error in dns response, got nil")
		}
		if !strings.Contains(err.Error(), "dns.routing.request") {
			t.Fatalf("error %q should point at the legal section", err.Error())
		}
	})
	t.Run("main routing", func(t *testing.T) {
		cfg := &Config{}
		cfg.Routing.Rules = []*config_parser.RoutingRule{
			mainRoutingOutboundRule("fakeip", "p"),
		}
		err := patchRoutingOutbound(cfg)
		if err == nil {
			t.Fatalf("expected error in main routing, got nil")
		}
		if !strings.Contains(err.Error(), "dns.routing.request") {
			t.Fatalf("error %q should point at the legal section", err.Error())
		}
	})
}

func TestPatchRoutingOutbound_RejectsCompoundUse(t *testing.T) {
	cfg := &Config{}
	rule := routingOutboundDnsRule("fakeip", "p")
	rule.AndFunctions = append(rule.AndFunctions,
		&config_parser.Function{Name: "qtype", Params: []*config_parser.Param{{Val: "A"}}})
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{rule}
	if err := patchRoutingOutbound(cfg); err == nil {
		t.Fatalf("expected error for compound use, got nil")
	}
}

func TestPatchRoutingOutbound_AllowsMissingSelector(t *testing.T) {
	// Configs without routing_outbound at all must be untouched.
	cfg := &Config{}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		{
			AndFunctions: []*config_parser.Function{
				{Name: "qname", Params: []*config_parser.Param{{Key: "suffix", Val: "cn"}}},
			},
			Outbound: config_parser.Function{Name: "alidns"},
		},
	}
	if err := patchRoutingOutbound(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
