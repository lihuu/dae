/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"os"
	"testing"
	"time"

	"github.com/daeuniverse/dae/component/daedns"
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

// writeConfigForTest writes content to path with mode 0600 so the config
// merger's permission check accepts it.
func writeConfigForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
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

func TestReloadManager_SetPendingStagedReload_PreservesCollectorForSingleSummary(t *testing.T) {
	log, hook := newCapturingLogger()
	collector := newSummaryCollector(log, rulesload.LifecycleReload)
	manager := newReloadManager(nil, nil, nil)
	handoff := &stagedReloadHandoff{}
	requestedAt := time.Now()

	manager.setPendingStagedReload(handoff, requestedAt, 42, collector)

	assert.Same(t, handoff, manager.currentPendingStagedHandoff())
	assert.Same(t, collector, manager.peekPendingReloadCollector())
	assert.Same(t, collector, manager.takePendingReloadCollector())
	assert.Nil(t, manager.takePendingReloadCollector())

	collector.Emit()
	summary := findEvent(hook.entries, rulesload.EventSummary)
	if summary == nil {
		t.Fatalf("expected a %q event", rulesload.EventSummary)
	}
	assert.Equal(t, rulesload.LifecycleReload, summary.Data["lifecycle"])
}

// TestSummaryCollector_EmitStage_RecordsAndEmits verifies that EmitStage
// both accumulates the duration into the summary AND publishes a structured
// rules_load_stage event on the collector's logger.
//
// This is the Observer-side contract that lets control_plane.go emit stage
// events (read_config / dns_controller_build / main_routing_optimize /
// main_routing_matcher_build) without having to thread a *logrus.Logger and
// the current lifecycle through every call site.
//
// Acceptance criterion AC-05 requires those stages to appear as
// rules_load_stage events on successful startup and reload; before this
// change, control_plane.go only called RecordStage and the events were
// silently lost.
func TestSummaryCollector_EmitStage_RecordsAndEmits(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleStartup)

	c.EmitStage(rulesload.StageDnsControllerBuild, 30, 0, 0, "")
	c.EmitStage(rulesload.StageMainRoutingOptimize, 100, 200, 220, "")
	c.EmitStage(rulesload.StageMainRoutingMatcher, 75, 0, 0, "")
	c.Emit()

	// Side A: stage events were emitted to the logger.
	stages := map[string]*logrus.Entry{}
	for _, e := range hook.entries {
		if v, ok := e.Data["event"]; ok && v == rulesload.EventStage {
			if s, ok := e.Data["stage"].(string); ok {
				stages[s] = e
			}
		}
	}
	assert.Contains(t, stages, rulesload.StageDnsControllerBuild)
	assert.Contains(t, stages, rulesload.StageMainRoutingOptimize)
	assert.Contains(t, stages, rulesload.StageMainRoutingMatcher)
	assert.Equal(t, rulesload.LifecycleStartup, stages[rulesload.StageDnsControllerBuild].Data["lifecycle"])
	assert.Equal(t, "ok", stages[rulesload.StageMainRoutingOptimize].Data["result"])
	assert.Equal(t, int64(100), stages[rulesload.StageMainRoutingOptimize].Data["duration_ms"])
	assert.Equal(t, 220, stages[rulesload.StageMainRoutingOptimize].Data["rules_out"])

	// Side B: durations also flowed into the summary (RecordStage equivalent).
	summary := findEvent(hook.entries, rulesload.EventSummary)
	if summary == nil {
		t.Fatalf("expected a %q event", rulesload.EventSummary)
	}
	assert.Equal(t, int64(30), summary.Data["dns_controller_build_ms"])
	assert.Equal(t, int64(100), summary.Data["main_routing_optimize_ms"])
	assert.Equal(t, int64(75), summary.Data["main_routing_matcher_build_ms"])
	assert.Equal(t, 220, summary.Data["main_routing_rules"])
}

// TestSummaryCollector_EmitStage_ErrorClass verifies that a non-empty
// errorClass causes the emitted stage event to carry result=error and the
// error_class field, matching the spec contract for failed stages.
func TestSummaryCollector_EmitStage_ErrorClass(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleReload)

	c.EmitStage(rulesload.StageDnsControllerBuild, 5, 0, 0, "dns_controller_build_error")

	stage := findEvent(hook.entries, rulesload.EventStage)
	if stage == nil {
		t.Fatalf("expected a %q event", rulesload.EventStage)
	}
	assert.Equal(t, "error", stage.Data["result"])
	assert.Equal(t, "dns_controller_build_error", stage.Data["error_class"])
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
		{
			AndFunctions: []*config_parser.Function{{
				Name:   "qname",
				Params: []*config_parser.Param{{Key: "suffix", Val: "local"}},
			}},
			Outbound: config_parser.Function{Name: "direct"},
		},
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
	// The final DNS request program contains the original direct rule plus the
	// one derived FakeIP rule.
	assert.Equal(t, 2, summary.Data["dns_request_rules"])
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

// TestSummaryCollector_RecordConfigStats_AggregatesChildDurations verifies
// that config child-stage durations and the four count fields produced by
// stats-aware config loading flow into the rules_load_summary event.
//
// Spec AC-08: every lifecycle summary must contain all six duration fields,
// config_unattributed_ms, and all four count fields.
func TestSummaryCollector_RecordConfigStats_AggregatesChildDurations(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleReload)

	stats := config.LoadStats{
		ReadFilesDuration:     12 * time.Millisecond,
		ParseDuration:         12840 * time.Millisecond,
		IncludeExpandDuration: 18 * time.Millisecond,
		MergeDuration:         45 * time.Millisecond,
		DecodeDuration:        190 * time.Millisecond,
		PatchDuration:         75 * time.Millisecond,
		IncludedFiles:         14,
		ConfigBytes:           482301,
		ParsedSections:        51,
		RawRoutingRules:       2067,
	}
	c.RecordConfigStats(stats)
	c.RecordConfigLoad(13215 * time.Millisecond)
	c.Emit()

	e := findEvent(hook.entries, rulesload.EventSummary)
	if e == nil {
		t.Fatalf("expected a %q log entry", rulesload.EventSummary)
	}
	assert.Equal(t, int64(12), e.Data["config_read_files_ms"])
	assert.Equal(t, int64(12840), e.Data["config_parse_ms"])
	assert.Equal(t, int64(18), e.Data["config_include_expand_ms"])
	assert.Equal(t, int64(45), e.Data["config_merge_ms"])
	assert.Equal(t, int64(190), e.Data["config_decode_ms"])
	assert.Equal(t, int64(75), e.Data["config_patch_ms"])
	// Spec AC-09: config_unattributed_ms = max(0, config_load_ms - sum).
	// config_load_ms = 13215, sum = 12 + 12840 + 18 + 45 + 190 + 75 = 13180.
	// Expected: 13215 - 13180 = 35.
	assert.Equal(t, int64(35), e.Data["config_unattributed_ms"])
	assert.Equal(t, 14, e.Data["included_files"])
	assert.Equal(t, int64(482301), e.Data["config_bytes"])
	assert.Equal(t, 51, e.Data["parsed_sections"])
	assert.Equal(t, 2067, e.Data["raw_routing_rules"])
}

// TestSummaryCollector_ConfigUnattributedMs_NeverNegative verifies AC-09's
// clamping behaviour: if the child stages happen to sum higher than the
// outer config_load_ms (clock skew, rounding), the unattributed field stays
// at 0 rather than going negative.
func TestSummaryCollector_ConfigUnattributedMs_NeverNegative(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleStartup)

	c.RecordConfigStats(config.LoadStats{
		ParseDuration: 1000 * time.Millisecond,
	})
	c.RecordConfigLoad(500 * time.Millisecond) // smaller than child sum
	c.Emit()

	e := findEvent(hook.entries, rulesload.EventSummary)
	if e == nil {
		t.Fatalf("expected a %q log entry", rulesload.EventSummary)
	}
	assert.Equal(t, int64(0), e.Data["config_unattributed_ms"])
}

// TestSummaryCollector_EmitConfigStages_EmitsOnePerCompletedStage verifies
// that a successful config load produces exactly one rules_load_stage event
// per completed child stage, each carrying the four config count fields.
//
// Spec AC-05: successful startup/reload must emit one event per completed
// child stage.
func TestSummaryCollector_EmitConfigStages_EmitsOnePerCompletedStage(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleStartup)

	stats := config.LoadStats{
		ReadFilesDuration:     12 * time.Millisecond,
		ParseDuration:         12840 * time.Millisecond,
		IncludeExpandDuration: 18 * time.Millisecond,
		MergeDuration:         45 * time.Millisecond,
		DecodeDuration:        190 * time.Millisecond,
		PatchDuration:         75 * time.Millisecond,
		IncludedFiles:         14,
		ConfigBytes:           482301,
		ParsedSections:        51,
		RawRoutingRules:       2067,
	}
	c.RecordConfigStats(stats)
	c.EmitConfigStages(stats)

	wantStages := map[string]int64{
		rulesload.StageConfigReadFiles:     12,
		rulesload.StageConfigParse:         12840,
		rulesload.StageConfigIncludeExpand: 18,
		rulesload.StageConfigMerge:         45,
		rulesload.StageConfigDecode:        190,
		rulesload.StageConfigPatch:         75,
	}
	seen := map[string]*logrus.Entry{}
	for _, e := range hook.entries {
		if v, ok := e.Data["event"]; !ok || v != rulesload.EventStage {
			continue
		}
		stage, _ := e.Data["stage"].(string)
		seen[stage] = e
	}
	for stage, dur := range wantStages {
		got, ok := seen[stage]
		if !ok {
			t.Fatalf("expected a stage event for %q", stage)
		}
		assert.Equal(t, dur, got.Data["duration_ms"], "stage %s duration", stage)
		assert.Equal(t, "ok", got.Data["result"], "stage %s result", stage)
		assert.Equal(t, rulesload.Lifecycle("startup"), got.Data["lifecycle"], "stage %s lifecycle", stage)
		// Spec: count fields appear on every event.
		assert.Equal(t, 14, got.Data["included_files"], "stage %s included_files", stage)
		assert.Equal(t, int64(482301), got.Data["config_bytes"], "stage %s config_bytes", stage)
		assert.Equal(t, 51, got.Data["parsed_sections"], "stage %s parsed_sections", stage)
		assert.Equal(t, 2067, got.Data["raw_routing_rules"], "stage %s raw_routing_rules", stage)
	}
}

// TestSummaryCollector_EmitConfigStages_FailureAttribution verifies AC-07:
// a failed config load produces a result=error event for the failing child
// stage (with error_class) and never emits the stages that didn't run.
func TestSummaryCollector_EmitConfigStages_FailureAttribution(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleReload)

	stats := config.LoadStats{
		ReadFilesDuration: 8 * time.Millisecond,
		ParseDuration:     3 * time.Millisecond,
		FailedStage:       config.StageConfigParse,
		IncludedFiles:     1,
		ConfigBytes:       128,
	}
	c.RecordConfigStats(stats)
	c.EmitConfigStages(stats)

	events := map[string]*logrus.Entry{}
	for _, e := range hook.entries {
		if v, ok := e.Data["event"]; !ok || v != rulesload.EventStage {
			continue
		}
		stage, _ := e.Data["stage"].(string)
		events[stage] = e
	}

	// config_read_files completed normally with a non-zero duration.
	read, ok := events[rulesload.StageConfigReadFiles]
	if !ok {
		t.Fatalf("expected stage event for %q", rulesload.StageConfigReadFiles)
	}
	assert.Equal(t, "ok", read.Data["result"])

	// config_parse is the failing stage; result=error, error_class is set.
	parse, ok := events[rulesload.StageConfigParse]
	if !ok {
		t.Fatalf("expected failing stage event for %q", rulesload.StageConfigParse)
	}
	assert.Equal(t, "error", parse.Data["result"])
	assert.Equal(t, "config_parse_error", parse.Data["error_class"])

	// Later stages with zero duration must not produce events.
	for _, stage := range []string{
		rulesload.StageConfigIncludeExpand,
		rulesload.StageConfigMerge,
		rulesload.StageConfigDecode,
		rulesload.StageConfigPatch,
	} {
		_, present := events[stage]
		assert.False(t, present, "expected no event for skipped stage %q", stage)
	}
}

// TestReadConfigWithStats_EndToEnd exercises the full
// readConfigWithStats() path against a real on-disk config file and
// verifies that the combined LoadStats has reasonable values for the
// stages that actually ran.
//
// This is the end-to-end glue between MergeWithStats / NewWithStats and the
// cmd-layer collector.
func TestReadConfigWithStats_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	entry := dir + "/config.dae"
	body := `global {}
routing {
  fallback: direct
  ip(8.8.8.8) -> direct
}
dns {
  upstream {
    cn: "udp://223.5.5.5:53"
  }
  routing {
    request {
      fallback: cn
    }
    response {
      fallback: accept
    }
  }
}
`
	if err := writeConfigForTest(entry, body); err != nil {
		t.Fatal(err)
	}

	conf, includes, stats, err := readConfigWithStats(entry)
	if err != nil {
		t.Fatalf("readConfigWithStats: %v", err)
	}
	assert.NotNil(t, conf)
	assert.Equal(t, []string{entry}, includes)
	assert.Equal(t, 1, stats.IncludedFiles)
	assert.Equal(t, int64(len(body)), stats.ConfigBytes)
	assert.GreaterOrEqual(t, stats.ParsedSections, 3)
	// fallback + the explicit ip(...) rule.
	assert.GreaterOrEqual(t, stats.RawRoutingRules, 1)
	assert.GreaterOrEqual(t, int64(stats.ReadFilesDuration), int64(0))
	assert.GreaterOrEqual(t, int64(stats.ParseDuration), int64(0))
	assert.GreaterOrEqual(t, int64(stats.DecodeDuration), int64(0))
	assert.GreaterOrEqual(t, int64(stats.PatchDuration), int64(0))
	assert.Empty(t, stats.FailedStage)

	// Avoid unused-variable warning from config_parser import in case the
	// test is the only consumer.
	_ = config_parser.Section{}
}

// TestSummaryCollector_EmitDaednsRouterStages_EmitsOnePerNonZeroStage
// verifies that the four daedns_router_build child stages each produce a
// rules_load_stage event when their BuildStats duration is non-zero, and
// that the same four durations accumulate into the rules_load_summary.
func TestSummaryCollector_EmitDaednsRouterStages_EmitsOnePerNonZeroStage(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleStartup)

	// EmitStage(daedns_router_build) is what cmd/run.go does today; we
	// invoke it here so the parent duration lands in the summary alongside
	// the child stages.
	c.EmitStage(rulesload.StageDaednsRouterBuild, 763, 0, 0, "")
	c.EmitDaednsRouterStages(daedns.BuildStats{
		RequestProgramNormalize: 612 * time.Millisecond,
		UpstreamInit:            5 * time.Millisecond,
		RequestMatcherBuild:     110 * time.Millisecond,
		MatchersCompile:         25 * time.Millisecond,
	})
	c.Emit()

	wantStages := map[string]int64{
		rulesload.StageDaednsRequestProgramNormalize: 612,
		rulesload.StageDaednsUpstreamInit:            5,
		rulesload.StageDaednsRequestMatcherBuild:     110,
		rulesload.StageDaednsMatchersCompile:         25,
	}
	seen := map[string]*logrus.Entry{}
	for _, e := range hook.entries {
		if v, ok := e.Data["event"]; !ok || v != rulesload.EventStage {
			continue
		}
		stage, _ := e.Data["stage"].(string)
		seen[stage] = e
	}
	for stage, dur := range wantStages {
		got, ok := seen[stage]
		if !ok {
			t.Fatalf("expected a stage event for %q", stage)
		}
		assert.Equal(t, dur, got.Data["duration_ms"], "stage %s duration", stage)
		assert.Equal(t, "ok", got.Data["result"], "stage %s result", stage)
		assert.Equal(t, rulesload.Lifecycle("startup"), got.Data["lifecycle"], "stage %s lifecycle", stage)
	}

	summary := findEvent(hook.entries, rulesload.EventSummary)
	if summary == nil {
		t.Fatalf("expected a %q log entry", rulesload.EventSummary)
	}
	assert.Equal(t, int64(763), summary.Data["daedns_router_build_ms"])
	assert.Equal(t, int64(612), summary.Data["daedns_request_program_normalize_ms"])
	assert.Equal(t, int64(5), summary.Data["daedns_upstream_init_ms"])
	assert.Equal(t, int64(110), summary.Data["daedns_request_matcher_build_ms"])
	assert.Equal(t, int64(25), summary.Data["daedns_matchers_compile_ms"])
	// 763 - (612 + 5 + 110 + 25) = 11
	assert.Equal(t, int64(11), summary.Data["daedns_router_unattributed_ms"])
}

// TestSummaryCollector_EmitDaednsRouterStages_ZeroDurationsSkipped verifies
// that substages with zero duration produce no rules_load_stage events.
func TestSummaryCollector_EmitDaednsRouterStages_ZeroDurationsSkipped(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleReload)

	c.EmitStage(rulesload.StageDaednsRouterBuild, 10, 0, 0, "")
	c.EmitDaednsRouterStages(daedns.BuildStats{
		RequestProgramNormalize: 7 * time.Millisecond,
		// other three intentionally zero
	})

	stageEvents := 0
	for _, e := range hook.entries {
		if v, ok := e.Data["event"]; !ok || v != rulesload.EventStage {
			continue
		}
		stage, _ := e.Data["stage"].(string)
		switch stage {
		case rulesload.StageDaednsRequestProgramNormalize,
			rulesload.StageDaednsUpstreamInit,
			rulesload.StageDaednsRequestMatcherBuild,
			rulesload.StageDaednsMatchersCompile:
			stageEvents++
		}
	}
	assert.Equal(t, 1, stageEvents,
		"only the one non-zero daedns child stage should emit an event")
}

// TestSummaryCollector_DaednsRouterUnattributedMs_NeverNegative verifies
// the snapshot clamps unattributed at 0 if children sum higher than the
// parent (clock skew / overlapping accumulation).
func TestSummaryCollector_DaednsRouterUnattributedMs_NeverNegative(t *testing.T) {
	log, hook := newCapturingLogger()
	c := newSummaryCollector(log, rulesload.LifecycleStartup)

	c.EmitStage(rulesload.StageDaednsRouterBuild, 50, 0, 0, "")
	c.EmitDaednsRouterStages(daedns.BuildStats{
		RequestProgramNormalize: 100 * time.Millisecond, // larger than parent
	})
	c.Emit()

	summary := findEvent(hook.entries, rulesload.EventSummary)
	if summary == nil {
		t.Fatalf("expected a %q log entry", rulesload.EventSummary)
	}
	assert.Equal(t, int64(0), summary.Data["daedns_router_unattributed_ms"])
}
