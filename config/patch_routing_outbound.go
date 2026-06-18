/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package config

import (
	"fmt"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/pkg/config_parser"
)

// routingOutboundFunctionName is duplicated from component/dns to keep the
// config package free of cross-package imports for one constant. Update both
// when the selector name changes — there is one cross-reference test (in
// component/dns) that exercises the runtime path so a divergence is caught.
const routingOutboundFunctionName = "routing_outbound"

// patchRoutingOutbound enforces the syntactic constraints on the
// `routing_outbound(...)` DNS request selector at parse time so `dae validate`
// catches misuse before reload. Outbound-existence (i.e. typo'd group names)
// requires the runtime outbound namespace and is checked later in the control
// plane during start/reload.
//
// Constraints checked here:
//   - The selector may appear ONLY in `dns.routing.request` rules.
//   - The rule's outbound MUST be `fakeip`.
//   - The rule's AndFunctions MUST consist of exactly one `routing_outbound`
//     function (no compound use, no negation, no mixing with `qname`/`qtype`).
//   - The function MUST take at least one positional argument.
func patchRoutingOutbound(params *Config) error {
	// Forbidden contexts: main routing, dns.routing.response.
	for i, rule := range params.Routing.Rules {
		if ruleNamesRoutingOutbound(rule) {
			return fmt.Errorf("routing rule[%d]: routing_outbound(...) is only valid in dns.routing.request, not in main routing", i)
		}
	}
	for i, rule := range params.Dns.Routing.Response.Rules {
		if ruleNamesRoutingOutbound(rule) {
			return fmt.Errorf("dns.routing.response rule[%d]: routing_outbound(...) is only valid in dns.routing.request", i)
		}
	}

	// Allowed context: dns.routing.request — verify shape.
	for i, rule := range params.Dns.Routing.Request.Rules {
		if rule == nil || !ruleNamesRoutingOutbound(rule) {
			continue
		}
		if rule.Outbound.Name != consts.DnsRequestOutboundIndex_FakeIP.String() {
			return fmt.Errorf("dns.routing.request rule[%d]: routing_outbound(...) must target outbound %q, got %q",
				i, consts.DnsRequestOutboundIndex_FakeIP.String(), rule.Outbound.Name)
		}
		if len(rule.AndFunctions) != 1 {
			return fmt.Errorf("dns.routing.request rule[%d]: routing_outbound(...) cannot be combined with other selectors", i)
		}
		fn := rule.AndFunctions[0]
		if fn.Not {
			return fmt.Errorf("dns.routing.request rule[%d]: !routing_outbound(...) is not supported", i)
		}
		if len(fn.Params) == 0 {
			return fmt.Errorf("dns.routing.request rule[%d]: routing_outbound(...) requires at least one outbound name", i)
		}
		for j, p := range fn.Params {
			if p.Key != "" {
				return fmt.Errorf("dns.routing.request rule[%d]: routing_outbound(...) takes positional outbound names, got key=%q at arg %d", i, p.Key, j)
			}
			if p.Val == "" {
				return fmt.Errorf("dns.routing.request rule[%d]: routing_outbound(...) has empty outbound name at arg %d", i, j)
			}
		}
	}
	return nil
}

func ruleNamesRoutingOutbound(rule *config_parser.RoutingRule) bool {
	if rule == nil {
		return false
	}
	for _, f := range rule.AndFunctions {
		if f != nil && f.Name == routingOutboundFunctionName {
			return true
		}
	}
	return false
}
