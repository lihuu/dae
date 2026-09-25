/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@daeuniverse.org>
 */

package dns

import (
	"errors"
	"strings"
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/sirupsen/logrus"
)

// outboundSet builds the existence predicate the expander needs.
// All tests pass an explicit outbound namespace so they don't drift with the
// default-config defaults.
func outboundSet(names ...string) func(string) bool {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return func(name string) bool {
		_, ok := m[name]
		return ok
	}
}

func discardLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(nopWriter{})
	l.SetLevel(logrus.PanicLevel)
	return l
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// mainRule is a small DSL for assembling routing rules in tests.
// outbound is the rule's outbound name; functions are AND'd inside the rule.
func mainRule(outbound string, functions ...*config_parser.Function) *config_parser.RoutingRule {
	return &config_parser.RoutingRule{
		AndFunctions: functions,
		Outbound:     config_parser.Function{Name: outbound},
	}
}

func domainFn(params ...*config_parser.Param) *config_parser.Function {
	return &config_parser.Function{Name: "domain", Params: params}
}

func notDomainFn(params ...*config_parser.Param) *config_parser.Function {
	return &config_parser.Function{Name: "domain", Not: true, Params: params}
}

func dportFn(value string) *config_parser.Function {
	return &config_parser.Function{Name: "dport", Params: []*config_parser.Param{{Val: value}}}
}

func dipFn(value string) *config_parser.Function {
	return &config_parser.Function{Name: "dip", Params: []*config_parser.Param{{Val: value}}}
}

// routingOutboundSelector creates a `routing_outbound(<names>)` DNS request rule
// with the given outbound (typically "fakeip" for valid uses; anything else
// must be rejected by the expander).
func routingOutboundSelector(outbound string, names ...string) *config_parser.RoutingRule {
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

// expectQName verifies the given rule is `qname(key: values...) -> outbound`.
func expectQName(t *testing.T, rule *config_parser.RoutingRule, expectOutbound, expectKey string, expectValues ...string) {
	t.Helper()
	if rule.Outbound.Name != expectOutbound {
		t.Fatalf("expected outbound %q, got %q (rule=%v)", expectOutbound, rule.Outbound.Name, rule.String(false, false, false))
	}
	if len(rule.AndFunctions) != 1 {
		t.Fatalf("expected 1 function, got %d (rule=%v)", len(rule.AndFunctions), rule.String(false, false, false))
	}
	fn := rule.AndFunctions[0]
	if fn.Name != "qname" {
		t.Fatalf("expected qname, got %q", fn.Name)
	}
	if fn.Not {
		t.Fatalf("expected non-negated qname, got Not=true")
	}
	if len(fn.Params) != len(expectValues) {
		t.Fatalf("expected %d params, got %d", len(expectValues), len(fn.Params))
	}
	for i, p := range fn.Params {
		if p.Key != expectKey {
			t.Fatalf("param %d: expected key %q, got %q", i, expectKey, p.Key)
		}
		if p.Val != expectValues[i] {
			t.Fatalf("param %d: expected value %q, got %q", i, expectValues[i], p.Val)
		}
	}
}

func TestExpandRoutingOutbound_NoSelector_PassesThrough(t *testing.T) {
	dnsRules := []*config_parser.RoutingRule{
		mainRule("alidns", &config_parser.Function{Name: "qname", Params: []*config_parser.Param{{Key: "suffix", Val: "cn"}}}),
	}
	mainRules := []*config_parser.RoutingRule{
		mainRule("proxy", domainFn(&config_parser.Param{Key: "suffix", Val: "github.com"})),
	}

	out, summary, err := ExpandRoutingOutboundSelectors(discardLogger(), dnsRules, mainRules, outboundSet("proxy"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 || out[0] != dnsRules[0] {
		t.Fatalf("expected pass-through, got %#v", out)
	}
	if summary.DerivedRules != 0 {
		t.Fatalf("expected zero derived rules, got %d", summary.DerivedRules)
	}
	if len(summary.FollowOutbounds) != 0 {
		t.Fatalf("expected no follow outbounds, got %v", summary.FollowOutbounds)
	}
}

func TestExpandRoutingOutbound_DerivesPureDomainRulesWithFollowedOutbound(t *testing.T) {
	mainRules := []*config_parser.RoutingRule{
		mainRule("proxy_fakeip_auto_canary",
			domainFn(&config_parser.Param{Key: "full", Val: "translate.google.com"})),
		mainRule("direct",
			domainFn(&config_parser.Param{Key: "suffix", Val: "cn"})),
		mainRule("proxy_fakeip_auto_canary",
			domainFn(&config_parser.Param{Key: "suffix", Val: "github.com"},
				&config_parser.Param{Key: "suffix", Val: "githubusercontent.com"})),
	}
	dnsRules := []*config_parser.RoutingRule{
		routingOutboundSelector("fakeip", "proxy_fakeip_auto_canary"),
	}

	out, summary, err := ExpandRoutingOutboundSelectors(
		discardLogger(), dnsRules, mainRules,
		outboundSet("proxy_fakeip_auto_canary", "direct"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 derived rules, got %d (%v)", len(out), summary)
	}
	expectQName(t, out[0], "fakeip", "full", "translate.google.com")
	expectQName(t, out[1], "fakeip", "suffix", "github.com", "githubusercontent.com")
	if summary.DerivedRules != 2 {
		t.Fatalf("expected DerivedRules=2, got %d", summary.DerivedRules)
	}
	if summary.SkippedOutbound != 1 {
		t.Fatalf("expected SkippedOutbound=1 (the direct rule), got %d", summary.SkippedOutbound)
	}
}

func TestExpandRoutingOutbound_SupportsAllFourDomainKeys(t *testing.T) {
	mainRules := []*config_parser.RoutingRule{
		mainRule("p", domainFn(&config_parser.Param{Key: "full", Val: "google.com"})),
		mainRule("p", domainFn(&config_parser.Param{Key: "suffix", Val: "github.com"})),
		mainRule("p", domainFn(&config_parser.Param{Key: "keyword", Val: "openai"})),
		mainRule("p", domainFn(&config_parser.Param{Key: "regex", Val: `^init.*\.push\.apple\.com$`})),
	}
	dnsRules := []*config_parser.RoutingRule{routingOutboundSelector("fakeip", "p")}

	out, summary, err := ExpandRoutingOutboundSelectors(discardLogger(), dnsRules, mainRules, outboundSet("p"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("expected 4 derived rules, got %d", len(out))
	}
	expectQName(t, out[0], "fakeip", "full", "google.com")
	expectQName(t, out[1], "fakeip", "suffix", "github.com")
	expectQName(t, out[2], "fakeip", "keyword", "openai")
	expectQName(t, out[3], "fakeip", "regex", `^init.*\.push\.apple\.com$`)
	if summary.DerivedRules != 4 {
		t.Fatalf("DerivedRules=%d", summary.DerivedRules)
	}
}

func TestExpandRoutingOutbound_SkipsCompoundRules(t *testing.T) {
	mainRules := []*config_parser.RoutingRule{
		mainRule("p",
			domainFn(&config_parser.Param{Key: "suffix", Val: "example.com"}),
			dportFn("443"),
		),
		mainRule("p",
			domainFn(&config_parser.Param{Key: "suffix", Val: "ok.example"})),
	}
	dnsRules := []*config_parser.RoutingRule{routingOutboundSelector("fakeip", "p")}

	out, summary, err := ExpandRoutingOutboundSelectors(discardLogger(), dnsRules, mainRules, outboundSet("p"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 derived rule, got %d", len(out))
	}
	expectQName(t, out[0], "fakeip", "suffix", "ok.example")
	if summary.SkippedCompound != 1 {
		t.Fatalf("expected SkippedCompound=1, got %d", summary.SkippedCompound)
	}
}

func TestExpandRoutingOutbound_SkipsNonDomainAndGeositeAndNegated(t *testing.T) {
	mainRules := []*config_parser.RoutingRule{
		mainRule("p", dipFn("8.8.8.8")),
		mainRule("p", domainFn(&config_parser.Param{Key: "geosite", Val: "cn"})),
		mainRule("p", notDomainFn(&config_parser.Param{Key: "suffix", Val: "skip.example"})),
		mainRule("p", domainFn(&config_parser.Param{Key: "suffix", Val: "keep.example"})),
	}
	dnsRules := []*config_parser.RoutingRule{routingOutboundSelector("fakeip", "p")}

	out, summary, err := ExpandRoutingOutboundSelectors(discardLogger(), dnsRules, mainRules, outboundSet("p"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected exactly 1 derived rule, got %d", len(out))
	}
	expectQName(t, out[0], "fakeip", "suffix", "keep.example")
	if summary.SkippedNonDomain != 1 {
		t.Fatalf("expected SkippedNonDomain=1 for dip, got %d", summary.SkippedNonDomain)
	}
	if summary.SkippedGeosite != 1 {
		t.Fatalf("expected SkippedGeosite=1, got %d", summary.SkippedGeosite)
	}
	// Negated `!domain(...)` is treated as compound (not a pure positive
	// domain selector) — counted under SkippedCompound to keep the public
	// vocabulary aligned with the spec.
	if summary.SkippedCompound != 1 {
		t.Fatalf("expected SkippedCompound=1 for negated domain, got %d", summary.SkippedCompound)
	}
}

func TestExpandRoutingOutbound_PreservesDnsRuleOrder(t *testing.T) {
	gmailRule := mainRule("foreign-dns-rule",
		&config_parser.Function{Name: "qname", Params: []*config_parser.Param{{Key: "suffix", Val: "gmail.com"}}})
	gmailRule.Outbound.Name = "foreign"
	fallbackRule := mainRule("alidns",
		&config_parser.Function{Name: "qname", Params: []*config_parser.Param{{Key: "suffix", Val: "cn"}}})

	dnsRules := []*config_parser.RoutingRule{
		gmailRule,
		routingOutboundSelector("fakeip", "p"),
		fallbackRule,
	}
	mainRules := []*config_parser.RoutingRule{
		mainRule("p", domainFn(&config_parser.Param{Key: "full", Val: "translate.google.com"})),
		mainRule("p", domainFn(&config_parser.Param{Key: "suffix", Val: "gmail.com"})),
	}

	out, _, err := ExpandRoutingOutboundSelectors(discardLogger(), dnsRules, mainRules, outboundSet("p"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The selector contributed 2 derived rules; the explicit gmail and
	// fallback rules must keep their original positions around them.
	if len(out) != 4 {
		t.Fatalf("expected 4 rules total, got %d", len(out))
	}
	if out[0] != gmailRule {
		t.Fatalf("expected gmail rule first, got %v", out[0].String(false, false, false))
	}
	expectQName(t, out[1], "fakeip", "full", "translate.google.com")
	expectQName(t, out[2], "fakeip", "suffix", "gmail.com")
	if out[3] != fallbackRule {
		t.Fatalf("expected fallback (alidns) rule last, got %v", out[3].String(false, false, false))
	}
}

func TestExpandRoutingOutbound_RejectsNonFakeIPOutbound(t *testing.T) {
	dnsRules := []*config_parser.RoutingRule{routingOutboundSelector("alidns", "p")}
	_, _, err := ExpandRoutingOutboundSelectors(discardLogger(), dnsRules, nil, outboundSet("p", "alidns"))
	if err == nil {
		t.Fatalf("expected error for non-fakeip outbound, got nil")
	}
	if !errors.Is(err, ErrRoutingOutboundOnlyFakeIP) {
		t.Fatalf("expected ErrRoutingOutboundOnlyFakeIP, got %v", err)
	}
}

func TestExpandRoutingOutbound_RejectsUnknownOutboundName(t *testing.T) {
	dnsRules := []*config_parser.RoutingRule{routingOutboundSelector("fakeip", "typo_proxy")}
	_, _, err := ExpandRoutingOutboundSelectors(discardLogger(), dnsRules, nil, outboundSet("proxy"))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, ErrRoutingOutboundUnknownOutbound) {
		t.Fatalf("expected ErrRoutingOutboundUnknownOutbound, got %v", err)
	}
	if !strings.Contains(err.Error(), "typo_proxy") {
		t.Fatalf("error %q should name the unknown outbound", err.Error())
	}
}

func TestExpandRoutingOutbound_RejectsEmptyArgList(t *testing.T) {
	dnsRules := []*config_parser.RoutingRule{routingOutboundSelector("fakeip")}
	_, _, err := ExpandRoutingOutboundSelectors(discardLogger(), dnsRules, nil, outboundSet())
	if err == nil {
		t.Fatalf("expected error for empty arg list, got nil")
	}
	if !errors.Is(err, ErrRoutingOutboundEmptyArgs) {
		t.Fatalf("expected ErrRoutingOutboundEmptyArgs, got %v", err)
	}
}

func TestExpandRoutingOutbound_RejectsNegatedSelector(t *testing.T) {
	rule := routingOutboundSelector("fakeip", "p")
	rule.AndFunctions[0].Not = true
	_, _, err := ExpandRoutingOutboundSelectors(discardLogger(), []*config_parser.RoutingRule{rule}, nil, outboundSet("p"))
	if err == nil {
		t.Fatalf("expected error for negated routing_outbound, got nil")
	}
}

func TestExpandRoutingOutbound_RejectsCompoundWithSelector(t *testing.T) {
	// routing_outbound(...) && qtype(A) -> fakeip is not allowed; the
	// selector is a whole-rule construct.
	rule := routingOutboundSelector("fakeip", "p")
	rule.AndFunctions = append(rule.AndFunctions,
		&config_parser.Function{Name: "qtype", Params: []*config_parser.Param{{Val: "A"}}})
	_, _, err := ExpandRoutingOutboundSelectors(discardLogger(), []*config_parser.RoutingRule{rule}, nil, outboundSet("p"))
	if err == nil {
		t.Fatalf("expected error for routing_outbound mixed with other selectors, got nil")
	}
}

func TestExpandRoutingOutbound_DebugEntriesRecordedPerDerivedRule(t *testing.T) {
	mainRules := []*config_parser.RoutingRule{
		mainRule("p", domainFn(&config_parser.Param{Key: "full", Val: "a.example"})),
		mainRule("p", domainFn(&config_parser.Param{Key: "suffix", Val: "b.example"},
			&config_parser.Param{Key: "suffix", Val: "c.example"})),
	}
	dnsRules := []*config_parser.RoutingRule{routingOutboundSelector("fakeip", "p")}

	_, summary, err := ExpandRoutingOutboundSelectors(discardLogger(), dnsRules, mainRules, outboundSet("p"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(summary.Derived) != 3 {
		t.Fatalf("expected 3 debug entries (1 + 2 grouped values), got %d", len(summary.Derived))
	}
	if summary.Derived[0].DomainKey != "full" || summary.Derived[0].Domain != "a.example" {
		t.Fatalf("debug[0]=%+v", summary.Derived[0])
	}
	if summary.Derived[1].DomainKey != "suffix" || summary.Derived[1].Domain != "b.example" {
		t.Fatalf("debug[1]=%+v", summary.Derived[1])
	}
	if summary.Derived[2].DomainKey != "suffix" || summary.Derived[2].Domain != "c.example" {
		t.Fatalf("debug[2]=%+v", summary.Derived[2])
	}
	for i, d := range summary.Derived {
		if d.RoutingOutbound != "p" {
			t.Fatalf("debug[%d].RoutingOutbound=%q", i, d.RoutingOutbound)
		}
	}
}
