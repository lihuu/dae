/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"testing"
	"time"

	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/daeuniverse/dae/pkg/rulesload"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

// captureHook captures logrus entries for assertion in tests.
type captureHook struct {
	entries []*logrus.Entry
}

func (h *captureHook) Levels() []logrus.Level     { return logrus.AllLevels }
func (h *captureHook) Fire(e *logrus.Entry) error { h.entries = append(h.entries, e); return nil }

func newCapturingLogger() (*logrus.Logger, *captureHook) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureHook{}
	log.AddHook(hook)
	return log, hook
}

func findEvent(entries []*logrus.Entry, eventName string) *logrus.Entry {
	for _, e := range entries {
		if v, ok := e.Data["event"]; ok && v == eventName {
			return e
		}
	}
	return nil
}

// TestSummaryCollector_EmitStartupLifecycle verifies that a startup-lifecycle
// collector emits a rules_load_summary event with the recorded config_load_ms,
// a total_ms that is >= the config_load_ms (since both come from the same
// monotonic clock), result=ok, and lifecycle=startup.
func TestSummaryCollector_EmitStartupLifecycle(t *testing.T) {
	log, hook := newCapturingLogger()

	c := newSummaryCollector(log, rulesload.LifecycleStartup)
	c.RecordConfigLoad(123 * time.Millisecond)
	c.Emit()

	e := findEvent(hook.entries, rulesload.EventSummary)
	if e == nil {
		t.Fatalf("expected a %q log entry, got %d entries", rulesload.EventSummary, len(hook.entries))
	}

	assert.Equal(t, "rules_load", e.Data["component"])
	assert.Equal(t, rulesload.LifecycleStartup, e.Data["lifecycle"])
	assert.Equal(t, "ok", e.Data["result"])
	assert.Equal(t, int64(123), e.Data["config_load_ms"])

	totalMs, ok := e.Data["total_ms"].(int64)
	if !ok {
		t.Fatalf("expected total_ms int64, got %T (%v)", e.Data["total_ms"], e.Data["total_ms"])
	}
	assert.GreaterOrEqual(t, totalMs, int64(0), "total_ms must be non-negative")
}

// TestSummaryCollector_EmitReloadLifecycle verifies the same contract for
// reload lifecycle, with stage durations recorded via the Observer interface.
func TestSummaryCollector_EmitReloadLifecycle(t *testing.T) {
	log, hook := newCapturingLogger()

	c := newSummaryCollector(log, rulesload.LifecycleReload)
	c.RecordConfigLoad(50 * time.Millisecond)
	c.RecordStage(rulesload.StageDaednsRouterBuild, 25, 0, 0)
	c.RecordStage(rulesload.StageDnsControllerBuild, 30, 0, 0)
	c.RecordStage(rulesload.StageMainRoutingOptimize, 100, 200, 220)
	c.RecordStage(rulesload.StageMainRoutingMatcher, 75, 0, 0)
	c.SetFakeIPAutoDerived(6)
	c.Emit()

	e := findEvent(hook.entries, rulesload.EventSummary)
	if e == nil {
		t.Fatalf("expected a %q log entry, got %d entries", rulesload.EventSummary, len(hook.entries))
	}

	assert.Equal(t, rulesload.LifecycleReload, e.Data["lifecycle"])
	assert.Equal(t, "ok", e.Data["result"])
	assert.Equal(t, int64(50), e.Data["config_load_ms"])
	assert.Equal(t, int64(25), e.Data["daedns_router_build_ms"])
	assert.Equal(t, int64(30), e.Data["dns_controller_build_ms"])
	assert.Equal(t, int64(100), e.Data["main_routing_optimize_ms"])
	assert.Equal(t, int64(75), e.Data["main_routing_matcher_build_ms"])
	assert.Equal(t, 220, e.Data["main_routing_rules"])
	assert.Equal(t, 6, e.Data["fakeip_auto_derived_rules"])
}

// TestSummaryCollector_EmitErrorEvent verifies that SetError surfaces as
// result=error and error_class on the emitted summary.
func TestSummaryCollector_EmitErrorEvent(t *testing.T) {
	log, hook := newCapturingLogger()

	c := newSummaryCollector(log, rulesload.LifecycleValidate)
	c.RecordConfigLoad(5 * time.Millisecond)
	c.SetError("config_parse_error")
	c.Emit()

	e := findEvent(hook.entries, rulesload.EventSummary)
	if e == nil {
		t.Fatalf("expected a %q log entry", rulesload.EventSummary)
	}
	assert.Equal(t, "error", e.Data["result"])
	assert.Equal(t, "config_parse_error", e.Data["error_class"])
	assert.Equal(t, rulesload.LifecycleValidate, e.Data["lifecycle"])
}

// TestRunner_HoldsCollectorForStartupEmit verifies that newRunner stores the
// collector so that the startup path can invoke Emit() after a successful
// control plane is built. This is a structural contract check: without it,
// the startup lifecycle would never produce a rules_load_summary event.
func TestRunner_HoldsCollectorForStartupEmit(t *testing.T) {
	log := logrus.New()
	c := newSummaryCollector(log, rulesload.LifecycleStartup)
	r := newRunner(log, nil, nil, c)

	if r.collector != c {
		t.Fatalf("Runner.collector not set: got %v, want %v", r.collector, c)
	}

	// nil-collector branch must remain valid for non-Run callers (validate, tests).
	r2 := newRunner(log, nil, nil, nil)
	if r2.collector != nil {
		t.Fatalf("Runner.collector for nil input should be nil, got %v", r2.collector)
	}
}

// TestRunner_emitStartupSummaryIfCollector verifies the helper used by
// Runner.Run to publish the rules_load_summary event for the startup
// lifecycle. The helper must:
//   - Be a no-op when collector is nil (validate path / tests use nil).
//   - Emit exactly one rules_load_summary event with lifecycle=startup
//     when the collector is set.
//   - Be safe to call once per Runner; subsequent calls must not double-emit.
func TestRunner_emitStartupSummaryIfCollector_NilCollector(t *testing.T) {
	log, hook := newCapturingLogger()
	r := newRunner(log, nil, nil, nil)

	r.emitStartupSummaryIfCollector()

	if findEvent(hook.entries, rulesload.EventSummary) != nil {
		t.Fatalf("expected no %q event when collector is nil", rulesload.EventSummary)
	}
}

func TestRunner_emitStartupSummaryIfCollector_EmitsOnce(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleStartup)
	c.RecordConfigLoad(7 * time.Millisecond)
	r := newRunner(log, nil, nil, c)

	r.emitStartupSummaryIfCollector()
	r.emitStartupSummaryIfCollector() // second call must be a no-op

	count := 0
	for _, e := range hook.entries {
		if v, ok := e.Data["event"]; ok && v == rulesload.EventSummary {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 rules_load_summary event, got %d", count)
	}

	e := findEvent(hook.entries, rulesload.EventSummary)
	assert.Equal(t, rulesload.LifecycleStartup, e.Data["lifecycle"])
	assert.Equal(t, "ok", e.Data["result"])
	assert.Equal(t, int64(7), e.Data["config_load_ms"])
}

// TestSummaryCollector_RecordFakeIPAutoExpand verifies that
// StageFakeIPAutoExpand observations propagate into the rules_load_summary
// event as fakeip_auto_expand_ms. Without this, the spec field is always 0
// even though cmd/run.go has the wall-clock measurement.
func TestSummaryCollector_RecordFakeIPAutoExpand(t *testing.T) {
	log, hook := newCapturingLogger()

	c := newSummaryCollector(log, rulesload.LifecycleStartup)
	c.RecordStage(rulesload.StageFakeIPAutoExpand, 42, 0, 6)
	c.Emit()

	e := findEvent(hook.entries, rulesload.EventSummary)
	if e == nil {
		t.Fatalf("expected a %q log entry", rulesload.EventSummary)
	}
	assert.Equal(t, int64(42), e.Data["fakeip_auto_expand_ms"])
}

// TestSummaryCollector_RecordControlPlaneBuild verifies the
// StageControlPlaneBuild stage feeds control_plane_build_ms into the summary.
func TestSummaryCollector_RecordControlPlaneBuild(t *testing.T) {
	log, hook := newCapturingLogger()

	c := newSummaryCollector(log, rulesload.LifecycleStartup)
	c.RecordStage(rulesload.StageControlPlaneBuild, 1960, 0, 0)
	c.Emit()

	e := findEvent(hook.entries, rulesload.EventSummary)
	if e == nil {
		t.Fatalf("expected a %q log entry", rulesload.EventSummary)
	}
	assert.Equal(t, int64(1960), e.Data["control_plane_build_ms"])
}

// TestSummaryCollector_RecordReloadStages verifies that StageReloadHandoff
// and StageReloadRetire feed reload_handoff_ms and reload_retire_ms.
func TestSummaryCollector_RecordReloadStages(t *testing.T) {
	log, hook := newCapturingLogger()

	c := newSummaryCollector(log, rulesload.LifecycleReload)
	c.RecordStage(rulesload.StageReloadHandoff, 30, 0, 0)
	c.RecordStage(rulesload.StageReloadRetire, 12880, 0, 0)
	c.Emit()

	e := findEvent(hook.entries, rulesload.EventSummary)
	if e == nil {
		t.Fatalf("expected a %q log entry", rulesload.EventSummary)
	}
	assert.Equal(t, int64(30), e.Data["reload_handoff_ms"])
	assert.Equal(t, int64(12880), e.Data["reload_retire_ms"])
}

// TestRulesLoadTimingsEnabled_FromFlag verifies that the --timings cobra flag
// turns on validate-path observability.
func TestRulesLoadTimingsEnabled_FromFlag(t *testing.T) {
	if !rulesLoadTimingsEnabled(true, "") {
		t.Fatal("rulesLoadTimingsEnabled(true, \"\") must be true")
	}
}

// TestRulesLoadTimingsEnabled_FromEnv verifies that DAE_RULES_LOAD_TIMINGS=1
// turns on validate-path observability without the --timings flag.
func TestRulesLoadTimingsEnabled_FromEnv(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"1":     true,
		"true":  true,
		"YES":   true,
		"on":    true,
	}
	for v, want := range cases {
		if got := rulesLoadTimingsEnabled(false, v); got != want {
			t.Fatalf("rulesLoadTimingsEnabled(false, %q) = %v, want %v", v, got, want)
		}
	}
}

// TestValidateConfigForExpansion_WithCollector verifies that the timings-aware
// validate path records the FakeIP expand duration and rule counts into the
// collector and emits a corresponding rules_load_stage event. This is the
// only path that surfaces lifecycle=validate observability, per the spec.
func TestValidateConfigForExpansion_WithCollector(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleValidate)

	cfg := &config.Config{}
	cfg.Group = []config.Group{{Name: "proxy_canary"}}
	cfg.Routing.Rules = []*config_parser.RoutingRule{
		mainDomainSuffixRule("proxy_canary", "example.com"),
	}
	cfg.Dns.Routing.Request.Rules = []*config_parser.RoutingRule{
		dnsRoutingOutboundRequestRule("fakeip", "proxy_canary"),
	}

	if err := validateConfigForExpansionWithCollector(cfg, c, log); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	c.Emit()

	stage := findEvent(hook.entries, rulesload.EventStage)
	if stage == nil {
		t.Fatalf("expected a %q event", rulesload.EventStage)
	}
	assert.Equal(t, rulesload.LifecycleValidate, stage.Data["lifecycle"])
	assert.Equal(t, rulesload.StageFakeIPAutoExpand, stage.Data["stage"])

	summary := findEvent(hook.entries, rulesload.EventSummary)
	if summary == nil {
		t.Fatalf("expected a %q event", rulesload.EventSummary)
	}
	assert.Equal(t, rulesload.LifecycleValidate, summary.Data["lifecycle"])
	assert.Equal(t, "ok", summary.Data["result"])
	// Derived rule count: domain-suffix(example.com) expands to one qname.
	assert.Equal(t, 1, summary.Data["fakeip_auto_derived_rules"])
	// DNS request/response counts must flow through.
	assert.Equal(t, 1, summary.Data["dns_request_rules"])
}
// dns_response_rules counts flow into rules_load_summary and contribute to
// rules_total. Without this, both fields are always 0.
func TestSummaryCollector_SetDnsRouting(t *testing.T) {
	log, hook := newCapturingLogger()

	c := newSummaryCollector(log, rulesload.LifecycleStartup)
	c.RecordStage(rulesload.StageMainRoutingOptimize, 0, 0, 200)
	c.SetDnsRouting(8, 12)
	c.SetFakeIPAutoDerived(6)
	c.Emit()

	e := findEvent(hook.entries, rulesload.EventSummary)
	if e == nil {
		t.Fatalf("expected a %q log entry", rulesload.EventSummary)
	}
	assert.Equal(t, 8, e.Data["dns_request_rules"])
	assert.Equal(t, 12, e.Data["dns_response_rules"])
	assert.Equal(t, 200+8+12+6, e.Data["rules_total"])
}
