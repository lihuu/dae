/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package dns

import (
	"errors"
	"fmt"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/sirupsen/logrus"
)

// FunctionRoutingOutbound is the DNS request selector that, at start/reload,
// expands into synthetic qname rules derived from the in-memory main routing
// configuration.
//
// It is NOT a runtime selector. By the time RequestMatcherBuilder runs, every
// occurrence has already been rewritten into qname(...) -> fakeip rules — so
// runtime DNS queries never call back into the main routing matcher.
const FunctionRoutingOutbound = "routing_outbound"

// Sentinel errors. Tests assert with errors.Is; callers (control plane) wrap
// these in user-facing validation messages.
var (
	ErrRoutingOutboundOnlyFakeIP      = errors.New("routing_outbound(...) must target outbound \"fakeip\"")
	ErrRoutingOutboundUnknownOutbound = errors.New("routing_outbound(...) references unknown outbound")
	ErrRoutingOutboundEmptyArgs       = errors.New("routing_outbound(...) must list at least one outbound name")
	ErrRoutingOutboundMisuse          = errors.New("routing_outbound(...) cannot be combined with other selectors or negation")
)

// ExpansionDerived records one derived domain for debug observability.
// Source rule index is into the main routing rules slice as passed in.
type ExpansionDerived struct {
	DomainKey       string
	Domain          string
	RoutingOutbound string
	DnsOutbound     string
	SourceRuleIndex int
}

// ExpansionSummary is the structured outcome of one expansion pass. Field
// names mirror the spec's required `event=fakeip_auto_expand` payload.
type ExpansionSummary struct {
	FollowOutbounds  []string
	DerivedRules     int
	SkippedNonDomain int
	SkippedGeosite   int
	SkippedCompound  int
	SkippedOutbound  int
	Result           string
	Derived          []ExpansionDerived
}

// ExpandRoutingOutboundSelectors rewrites every routing_outbound(...) -> fakeip
// rule in dnsRequestRules into synthetic qname(...) -> fakeip rules derived
// from mainRoutingRules. Position of derived rules == position of the original
// selector. All other DNS request rules are returned unchanged in their
// original positions.
//
// outboundExists is the membership predicate over the outbound/group namespace
// loaded from the main config. Selector args are validated against it.
//
// The returned slice is a fresh slice; the input dnsRequestRules slice is not
// mutated. Individual *RoutingRule values that were not touched are returned
// by reference (no deep copy) — this matches how SplitRequestRules treats
// pass-through rules elsewhere in this package.
func ExpandRoutingOutboundSelectors(
	log *logrus.Logger,
	dnsRequestRules []*config_parser.RoutingRule,
	mainRoutingRules []*config_parser.RoutingRule,
	outboundExists func(name string) bool,
) ([]*config_parser.RoutingRule, ExpansionSummary, error) {
	summary := ExpansionSummary{Result: "ok"}

	// First pass: validate all routing_outbound(...) selectors and collect
	// the union of followed outbounds. We fail fast on any invalid selector
	// so the caller can surface a clean error before doing real work.
	var allFollow []string
	seenFollow := make(map[string]struct{})
	for _, rule := range dnsRequestRules {
		if !ruleHasRoutingOutbound(rule) {
			continue
		}
		if err := validateRoutingOutboundRule(rule, outboundExists); err != nil {
			summary.Result = "error"
			return nil, summary, err
		}
		for _, p := range rule.AndFunctions[0].Params {
			if _, dup := seenFollow[p.Val]; dup {
				continue
			}
			seenFollow[p.Val] = struct{}{}
			allFollow = append(allFollow, p.Val)
		}
	}
	summary.FollowOutbounds = allFollow

	// No selector → nothing to do, return the input slice as-is.
	if len(allFollow) == 0 {
		return dnsRequestRules, summary, nil
	}

	out := make([]*config_parser.RoutingRule, 0, len(dnsRequestRules))
	for _, rule := range dnsRequestRules {
		if !ruleHasRoutingOutbound(rule) {
			out = append(out, rule)
			continue
		}
		// Per-selector follow set — preserves the spec's "the listed
		// outbound names" semantics if multiple selectors are used.
		ruleFollow := make(map[string]struct{}, len(rule.AndFunctions[0].Params))
		for _, p := range rule.AndFunctions[0].Params {
			ruleFollow[p.Val] = struct{}{}
		}
		before := len(out)
		derived := deriveRules(log, mainRoutingRules, ruleFollow, &summary)
		out = append(out, derived...)
		summary.DerivedRules += len(out) - before
	}
	return out, summary, nil
}

// ruleHasRoutingOutbound returns true iff the rule's first (and only)
// function is routing_outbound. Compound or mixed forms are caught by
// validateRoutingOutboundRule below; this predicate is the dispatch trigger.
func ruleHasRoutingOutbound(rule *config_parser.RoutingRule) bool {
	if rule == nil || len(rule.AndFunctions) == 0 {
		return false
	}
	for _, f := range rule.AndFunctions {
		if f.Name == FunctionRoutingOutbound {
			return true
		}
	}
	return false
}

func validateRoutingOutboundRule(rule *config_parser.RoutingRule, outboundExists func(string) bool) error {
	if rule.Outbound.Name != consts.DnsRequestOutboundIndex_FakeIP.String() {
		return fmt.Errorf("%w: got -> %s", ErrRoutingOutboundOnlyFakeIP, rule.Outbound.Name)
	}
	if len(rule.AndFunctions) != 1 {
		return fmt.Errorf("%w: rule has %d AND'd selectors, expected exactly 1",
			ErrRoutingOutboundMisuse, len(rule.AndFunctions))
	}
	fn := rule.AndFunctions[0]
	if fn.Name != FunctionRoutingOutbound {
		return fmt.Errorf("%w: function name %q", ErrRoutingOutboundMisuse, fn.Name)
	}
	if fn.Not {
		return fmt.Errorf("%w: negation (!routing_outbound) is not supported", ErrRoutingOutboundMisuse)
	}
	if len(fn.Params) == 0 {
		return ErrRoutingOutboundEmptyArgs
	}
	for _, p := range fn.Params {
		if p.Key != "" {
			return fmt.Errorf("%w: routing_outbound takes positional outbound names, got key=%q", ErrRoutingOutboundMisuse, p.Key)
		}
		if p.Val == "" {
			return fmt.Errorf("%w: empty outbound name", ErrRoutingOutboundEmptyArgs)
		}
		if !outboundExists(p.Val) {
			return fmt.Errorf("%w: %q", ErrRoutingOutboundUnknownOutbound, p.Val)
		}
	}
	return nil
}

// deriveRules walks the main routing rules in order and produces synthetic
// qname rules for every eligible source rule. summary's skip counters are
// incremented for each ineligible rule.
func deriveRules(
	log *logrus.Logger,
	mainRoutingRules []*config_parser.RoutingRule,
	follow map[string]struct{},
	summary *ExpansionSummary,
) []*config_parser.RoutingRule {
	var out []*config_parser.RoutingRule
	dnsOutbound := consts.DnsRequestOutboundIndex_FakeIP.String()
	for idx, rule := range mainRoutingRules {
		category := classifyMainRule(rule)
		switch category {
		case mainRulePureDomain:
			// Eligible only when its outbound is followed.
			if _, ok := follow[rule.Outbound.Name]; !ok {
				summary.SkippedOutbound++
				continue
			}
			fn := rule.AndFunctions[0]
			// Group params by key, preserving original key order, to
			// produce one synthetic rule per (rule, key) pair.
			keys, byKey := groupDomainParamsByKey(fn.Params)
			for _, key := range keys {
				values := byKey[key]
				params := make([]*config_parser.Param, len(values))
				for i, v := range values {
					params[i] = &config_parser.Param{Key: key, Val: v}
					summary.Derived = append(summary.Derived, ExpansionDerived{
						DomainKey:       key,
						Domain:          v,
						RoutingOutbound: rule.Outbound.Name,
						DnsOutbound:     dnsOutbound,
						SourceRuleIndex: idx,
					})
				}
				out = append(out, &config_parser.RoutingRule{
					AndFunctions: []*config_parser.Function{
						{Name: consts.Function_QName, Params: params},
					},
					Outbound: config_parser.Function{Name: dnsOutbound},
				})
			}
		case mainRuleGeositeDomain:
			summary.SkippedGeosite++
		case mainRuleCompound:
			summary.SkippedCompound++
		case mainRuleNonDomain:
			summary.SkippedNonDomain++
		}
	}
	if log != nil && log.Level >= logrus.DebugLevel {
		log.Debugf("routing_outbound: derived %d qname rules from %d main routing rules",
			len(out), len(mainRoutingRules))
	}
	return out
}

type mainRuleCategory int

const (
	mainRulePureDomain mainRuleCategory = iota
	mainRuleGeositeDomain
	mainRuleCompound
	mainRuleNonDomain
)

// classifyMainRule decides whether a main routing rule is eligible to derive
// a DNS qname rule from. We require:
//   - exactly one AndFunction (no compound)
//   - that function is `domain` (not `dip`, `dport`, `pname`, `sip`, ...)
//   - that function is NOT negated
//   - every Param.Key is one of full|suffix|keyword|regex
//
// A `domain(geosite: ...)` rule is reported separately so it counts under the
// spec's `skipped_geosite` rather than `skipped_non_domain`.
func classifyMainRule(rule *config_parser.RoutingRule) mainRuleCategory {
	if rule == nil {
		return mainRuleNonDomain
	}
	if len(rule.AndFunctions) != 1 {
		return mainRuleCompound
	}
	fn := rule.AndFunctions[0]
	if fn.Name != consts.Function_Domain {
		return mainRuleNonDomain
	}
	if fn.Not {
		// Negated `!domain(...)` is structurally pure-domain but the
		// spec lists negated/compound forms together as not derivable;
		// we report it under SkippedCompound to keep the public
		// vocabulary as the spec writes it.
		return mainRuleCompound
	}
	if len(fn.Params) == 0 {
		return mainRuleCompound
	}
	hasGeosite := false
	for _, p := range fn.Params {
		switch consts.RoutingDomainKey(p.Key) {
		case consts.RoutingDomainKey_Full,
			consts.RoutingDomainKey_Keyword,
			consts.RoutingDomainKey_Suffix,
			consts.RoutingDomainKey_Regex:
		case "geosite":
			hasGeosite = true
		default:
			// Unknown key — skip as compound to stay conservative.
			return mainRuleCompound
		}
	}
	if hasGeosite {
		return mainRuleGeositeDomain
	}
	return mainRulePureDomain
}

// groupDomainParamsByKey groups param values by key while preserving the
// first-occurrence order of keys, matching how the parser stores `domain(suffix: a, suffix: b)`.
func groupDomainParamsByKey(params []*config_parser.Param) (keys []string, byKey map[string][]string) {
	byKey = make(map[string][]string)
	for _, p := range params {
		if _, ok := byKey[p.Key]; !ok {
			keys = append(keys, p.Key)
		}
		byKey[p.Key] = append(byKey[p.Key], p.Val)
	}
	return keys, byKey
}
