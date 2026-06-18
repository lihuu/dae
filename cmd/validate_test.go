/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"strings"
	"testing"

	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/pkg/config_parser"
)

// dnsRoutingOutboundRequestRule builds a single
// `routing_outbound(<names...>) -> <target>` DNS request rule for tests.
func dnsRoutingOutboundRequestRule(target string, names ...string) *config_parser.RoutingRule {
	params := make([]*config_parser.Param, len(names))
	for i, n := range names {
		params[i] = &config_parser.Param{Val: n}
	}
	return &config_parser.RoutingRule{
		AndFunctions: []*config_parser.Function{
			{Name: "routing_outbound", Params: params},
		},
		Outbound: config_parser.Function{Name: target},
	}
}

func mainDomainSuffixRule(outbound, suffix string) *config_parser.RoutingRule {
	return &config_parser.RoutingRule{
		AndFunctions: []*config_parser.Function{{
			Name:   "domain",
			Params: []*config_parser.Param{{Key: "suffix", Val: suffix}},
		}},
		Outbound: config_parser.Function{Name: outbound},
	}
}

// TestValidateExpansion_NoSelector confirms validate is a no-op when no
// `routing_outbound(...)` selector is present (the common case for configs
// that don't use the FakeIP auto-derivation feature).
func TestValidateExpansion_NoSelector(t *testing.T) {
	cfg := &config.Config{}
	cfg.Group = []config.Group{{Name: "proxy"}}
	cfg.Routing.Rules = []*config_parser.RoutingRule{
		mainDomainSuffixRule("proxy", "example.com"),
	}
	if err := validateConfigForExpansion(cfg); err != nil {
		t.Fatalf("expected no error for selector-free config, got %v", err)
	}
}

// TestValidateExpansion_RejectsUnknownOutbound is the regression test for
// the spec hazard: typo'd outbound names must fail at `dae validate`,
// not slip through and only fail at reload.
func TestValidateExpansion_RejectsUnknownOutbound(t *testing.T) {
	cfg := &config.Config{}
	cfg.Group = []config.Group{{Name: "proxy"}}
	cfg.Routing.Rules = []*config_parser.RoutingRule{
		mainDomainSuffixRule("proxy", "example.com"),
	}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		dnsRoutingOutboundRequestRule("fakeip", "proxy_typo"),
	}
	err := validateConfigForExpansion(cfg)
	if err == nil {
		t.Fatalf("expected error for unknown outbound, got nil")
	}
	if !strings.Contains(err.Error(), "proxy_typo") {
		t.Fatalf("error %q should name the unknown outbound", err.Error())
	}
}

// TestValidateExpansion_RejectsNonFakeIPTarget catches `routing_outbound(...) -> direct`
// at validate time. (The parse-time patch in config/patch_routing_outbound.go
// also catches this; this test ensures the validate path agrees, so neither
// path can drift away from the other.)
func TestValidateExpansion_RejectsNonFakeIPTarget(t *testing.T) {
	cfg := &config.Config{}
	cfg.Group = []config.Group{{Name: "proxy"}}
	cfg.Routing.Rules = []*config_parser.RoutingRule{
		mainDomainSuffixRule("proxy", "example.com"),
	}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		dnsRoutingOutboundRequestRule("direct", "proxy"),
	}
	err := validateConfigForExpansion(cfg)
	if err == nil {
		t.Fatalf("expected error for non-fakeip target, got nil")
	}
}

// TestValidateExpansion_RejectsSelectorWithEmptyMainRouting catches the
// degenerate case where a selector exists but the main routing slice is
// empty. Without this check, a config that compiles cleanly but has no
// derivable rules would only emit `result=ok derived_rules=0` at reload.
// See the regression note on errEmptyMainRoutingRulesWithSelector in
// control/fakeip_routing_outbound.go.
func TestValidateExpansion_RejectsSelectorWithEmptyMainRouting(t *testing.T) {
	cfg := &config.Config{}
	cfg.Group = []config.Group{{Name: "proxy"}}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		dnsRoutingOutboundRequestRule("fakeip", "proxy"),
	}
	err := validateConfigForExpansion(cfg)
	if err == nil {
		t.Fatalf("expected error for selector with empty main routing, got nil")
	}
	if !strings.Contains(err.Error(), "main routing rules") {
		t.Fatalf("error %q should mention empty main routing rules", err.Error())
	}
}

// TestValidateExpansion_AcceptsKnownOutbound confirms a well-formed config
// with a real outbound name + non-empty main routing passes validate.
func TestValidateExpansion_AcceptsKnownOutbound(t *testing.T) {
	cfg := &config.Config{}
	cfg.Group = []config.Group{{Name: "proxy"}, {Name: "proxy_canary"}}
	cfg.Routing.Rules = []*config_parser.RoutingRule{
		mainDomainSuffixRule("proxy_canary", "example.com"),
	}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		dnsRoutingOutboundRequestRule("fakeip", "proxy_canary"),
	}
	if err := validateConfigForExpansion(cfg); err != nil {
		t.Fatalf("expected no error for valid config, got %v", err)
	}
}

// TestValidateExpansion_BuiltInOutboundsAreKnown confirms `direct` and
// `block` are recognized without needing a corresponding group entry.
func TestValidateExpansion_BuiltInOutboundsAreKnown(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Rules = []*config_parser.RoutingRule{
		mainDomainSuffixRule("direct", "example.com"),
	}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		dnsRoutingOutboundRequestRule("fakeip", "direct"),
	}
	if err := validateConfigForExpansion(cfg); err != nil {
		t.Fatalf("expected no error when selector references the built-in `direct` outbound, got %v", err)
	}
}
