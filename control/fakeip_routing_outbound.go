/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"errors"

	"github.com/daeuniverse/dae/component/dns"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/sirupsen/logrus"
)

// errEmptyMainRoutingRulesWithSelector is returned when at least one
// `routing_outbound(...) -> fakeip` selector is present but the caller
// passed an empty main-routing-rules slice. See the regression note in
// expandFakeIPRoutingOutbound for the production hazard this guards.
var errEmptyMainRoutingRulesWithSelector = errors.New(
	"routing_outbound(...) selector present but main routing rules slice is empty; " +
		"this would silently expand to zero FakeIP rules — caller must pass the parsed routing rules before they are released",
)

// ExpandFakeIPRoutingOutbound rewrites every routing_outbound(...) -> fakeip
// rule in dnsRequestRules into synthetic qname rules derived from
// mainRoutingRules, then emits the structured `fakeip_auto_expand` summary
// event and per-derived-domain `fakeip_auto_derived` debug events that the
// FakeIP observability spec requires.
//
// Behavior:
//   - No selector present: input is returned unchanged and no events are
//     emitted (caller's start/reload path is otherwise unchanged).
//   - At least one selector: the summary event is always emitted (with
//     result="ok" or result="error"), so operators can see in the log that
//     expansion ran. Per-domain debug events are gated by the logger's
//     debug level to keep production volume manageable.
//
// The function calls dns.ExpandRoutingOutboundSelectors for the actual
// AST rewrite; this wrapper exists in the control package because the
// observability events are expected to live alongside the other fakeip_*
// events emitted by ControlPlane / DnsController.
//
// CRITICAL CALL-SITE NOTE: this MUST run AFTER config parse and patch but
// BEFORE any consumer reads dns.routing.request.Rules — daedns.NewWithOption
// AND control.NewControlPlaneWithContext both consume it through
// componentdns.NewNormalizedRequestRoutingProgram, which has no parser
// registered for `routing_outbound` and would fail with
// "unknown function: routing_outbound". cmd/run.go owns that boundary today.
func ExpandFakeIPRoutingOutbound(
	log *logrus.Logger,
	dnsRequestRules []*config_parser.RoutingRule,
	mainRoutingRules []*config_parser.RoutingRule,
	outboundExists func(name string) bool,
) ([]*config_parser.RoutingRule, error) {
	out, _, err := expandFakeIPRoutingOutboundWithSummary(log, dnsRequestRules, mainRoutingRules, outboundExists)
	return out, err
}

// ExpandFakeIPRoutingOutboundWithSummary performs the same rewrite as
// ExpandFakeIPRoutingOutbound and also returns the expansion counters used by
// load-time observability.
func ExpandFakeIPRoutingOutboundWithSummary(
	log *logrus.Logger,
	dnsRequestRules []*config_parser.RoutingRule,
	mainRoutingRules []*config_parser.RoutingRule,
	outboundExists func(name string) bool,
) ([]*config_parser.RoutingRule, dns.ExpansionSummary, error) {
	return expandFakeIPRoutingOutboundWithSummary(log, dnsRequestRules, mainRoutingRules, outboundExists)
}

func expandFakeIPRoutingOutbound(
	log *logrus.Logger,
	dnsRequestRules []*config_parser.RoutingRule,
	mainRoutingRules []*config_parser.RoutingRule,
	outboundExists func(name string) bool,
) ([]*config_parser.RoutingRule, error) {
	out, _, err := expandFakeIPRoutingOutboundWithSummary(log, dnsRequestRules, mainRoutingRules, outboundExists)
	return out, err
}

func expandFakeIPRoutingOutboundWithSummary(
	log *logrus.Logger,
	dnsRequestRules []*config_parser.RoutingRule,
	mainRoutingRules []*config_parser.RoutingRule,
	outboundExists func(name string) bool,
) ([]*config_parser.RoutingRule, dns.ExpansionSummary, error) {
	hasSelector := false
	for _, r := range dnsRequestRules {
		if r == nil {
			continue
		}
		for _, f := range r.AndFunctions {
			if f != nil && f.Name == dns.FunctionRoutingOutbound {
				hasSelector = true
				break
			}
		}
		if hasSelector {
			break
		}
	}
	if !hasSelector {
		return dnsRequestRules, dns.ExpansionSummary{Result: "ok"}, nil
	}

	// Validate the selector(s) first — this catches non-fakeip targets,
	// unknown outbound names, empty arg lists, negation, and compound use,
	// independent of how many main routing rules we got. The dns-package
	// expander short-circuits on validation errors before walking the main
	// rules, so an empty main slice does not mask a typed selector error.
	out, summary, err := dns.ExpandRoutingOutboundSelectors(log, dnsRequestRules, mainRoutingRules, outboundExists)
	if err != nil {
		logFakeIPAutoExpand(log, summary, err)
		return nil, summary, err
	}

	// After validation passes, guard against the failure mode in which the
	// selector is well-formed but mainRoutingRules has been released or
	// never populated. Without this check the expansion silently produces
	// zero derived rules with result=ok, which looks indistinguishable
	// from "no eligible rules in the routing config" — and ships canary
	// deployments with the FakeIP allowlist quietly empty. Fail loudly.
	if len(mainRoutingRules) == 0 {
		err := errEmptyMainRoutingRulesWithSelector
		// Reuse the followed-outbounds list collected during validation so
		// operators can see WHICH selectors hit the empty-input path.
		summary.Result = "error"
		logFakeIPAutoExpand(log, summary, err)
		return nil, summary, err
	}

	logFakeIPAutoExpand(log, summary, nil)
	logFakeIPAutoDerived(log, summary)
	return out, summary, nil
}

// logFakeIPAutoExpand emits the structured summary event for one expansion
// pass. It is always called when at least one routing_outbound(...) selector
// was present, regardless of success.
func logFakeIPAutoExpand(log *logrus.Logger, summary dns.ExpansionSummary, err error) {
	if log == nil {
		return
	}
	fields := fakeipEventBase("fakeip_auto_expand")
	fields["selector"] = dns.FunctionRoutingOutbound
	follow := summary.FollowOutbounds
	if follow == nil {
		follow = []string{}
	}
	fields["follow_outbounds"] = follow
	fields["derived_rules"] = summary.DerivedRules
	fields["skipped_non_domain"] = summary.SkippedNonDomain
	fields["skipped_geosite"] = summary.SkippedGeosite
	fields["skipped_compound"] = summary.SkippedCompound
	fields["skipped_outbound"] = summary.SkippedOutbound
	if err != nil {
		fields["result"] = "error"
		fields["error"] = err.Error()
		log.WithFields(fields).Warn("fakeip_event")
		return
	}
	if summary.Result == "" {
		fields["result"] = "ok"
	} else {
		fields["result"] = summary.Result
	}
	log.WithFields(fields).Info("fakeip_event")
}

// logFakeIPAutoDerived emits one debug event per derived domain. Gated by
// the logger's debug level so production-default Info logs stay quiet.
func logFakeIPAutoDerived(log *logrus.Logger, summary dns.ExpansionSummary) {
	if log == nil || !log.IsLevelEnabled(logrus.DebugLevel) {
		return
	}
	for _, d := range summary.Derived {
		fields := fakeipEventBase("fakeip_auto_derived")
		fields["domain_key"] = d.DomainKey
		fields["domain"] = d.Domain
		fields["routing_outbound"] = d.RoutingOutbound
		fields["dns_outbound"] = d.DnsOutbound
		fields["source_rule_index"] = d.SourceRuleIndex
		log.WithFields(fields).Debug("fakeip_event")
	}
}
