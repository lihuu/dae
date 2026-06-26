package rulesload

import (
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

// captureLogHook captures all logrus entries for assertion.
type captureLogHook struct {
	entries []*logrus.Entry
}

func (h *captureLogHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *captureLogHook) Fire(entry *logrus.Entry) error {
	h.entries = append(h.entries, entry)
	return nil
}

func TestEmitStage_HasRequiredFields(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitStage(log, LifecycleStartup, StageReadConfig, 1234, 5, 10, "")

	if len(hook.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.entries))
	}
	e := hook.entries[0]
	assert.Equal(t, "rules_load", e.Data["component"])
	assert.Equal(t, "rules_load_stage", e.Data["event"])
	assert.Equal(t, 1, e.Data["event_version"])
	assert.Equal(t, Lifecycle("startup"), e.Data["lifecycle"])
	assert.Equal(t, "read_config", e.Data["stage"])
	assert.Equal(t, int64(1234), e.Data["duration_ms"])
	assert.Equal(t, 5, e.Data["rules_in"])
	assert.Equal(t, 10, e.Data["rules_out"])
	assert.Equal(t, "ok", e.Data["result"])
}

func TestEmitStage_ErrorEvent(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitStage(log, LifecycleReload, StageFakeIPAutoExpand, 567, 8, 3, "expand_failed")

	if len(hook.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.entries))
	}
	e := hook.entries[0]
	assert.Equal(t, "error", e.Data["result"])
	assert.Equal(t, "expand_failed", e.Data["error_class"])
	assert.Equal(t, Lifecycle("reload"), e.Data["lifecycle"])
}

func TestEmitSummary_HasRequiredFields(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitSummary(log, Summary{
		Lifecycle:             LifecycleStartup,
		TotalMs:               29120,
		ConfigLoadMs:          13840,
		FakeIPAutoExpandMs:    120,
		DaednsRouterBuildMs:   420,
		DnsControllerBuildMs:  380,
		MainRoutingOptimizeMs: 5600,
		MainRoutingMatcherMs:  1560,
		ControlPlaneBuildMs:   1960,
		ReloadHandoffMs:       0,
		ReloadRetireMs:        0,
		RulesTotal:            2220,
		MainRoutingRules:      2200,
		DnsRequestRules:       8,
		DnsResponseRules:      12,
		FakeIPAutoDerived:     6,
	})

	if len(hook.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.entries))
	}
	e := hook.entries[0]
	assert.Equal(t, "rules_load", e.Data["component"])
	assert.Equal(t, "rules_load_summary", e.Data["event"])
	assert.Equal(t, 1, e.Data["event_version"])
	assert.Equal(t, Lifecycle("startup"), e.Data["lifecycle"])
	assert.Equal(t, "ok", e.Data["result"])
	assert.Equal(t, int64(29120), e.Data["total_ms"])
	assert.Equal(t, int64(13840), e.Data["config_load_ms"])
	assert.Equal(t, int64(120), e.Data["fakeip_auto_expand_ms"])
	assert.Equal(t, int64(420), e.Data["daedns_router_build_ms"])
	assert.Equal(t, int64(380), e.Data["dns_controller_build_ms"])
	assert.Equal(t, int64(5600), e.Data["main_routing_optimize_ms"])
	assert.Equal(t, int64(1560), e.Data["main_routing_matcher_build_ms"])
	assert.Equal(t, int64(1960), e.Data["control_plane_build_ms"])
	assert.Equal(t, int64(0), e.Data["reload_handoff_ms"])
	assert.Equal(t, int64(0), e.Data["reload_retire_ms"])
	assert.Equal(t, 2220, e.Data["rules_total"])
	assert.Equal(t, 2200, e.Data["main_routing_rules"])
	assert.Equal(t, 8, e.Data["dns_request_rules"])
	assert.Equal(t, 12, e.Data["dns_response_rules"])
	assert.Equal(t, 6, e.Data["fakeip_auto_derived_rules"])
}

func TestEmitSummary_WithError(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitSummary(log, Summary{
		Lifecycle:    LifecycleValidate,
		ErrorClass:   "config_parse_error",
		TotalMs:      500,
		ConfigLoadMs: 500,
	})

	if len(hook.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.entries))
	}
	e := hook.entries[0]
	assert.Equal(t, "error", e.Data["result"])
	assert.Equal(t, "config_parse_error", e.Data["error_class"])
	assert.Equal(t, Lifecycle("validate"), e.Data["lifecycle"])
}

func TestEmitStage_AllLifecycles(t *testing.T) {
	for _, lc := range []Lifecycle{LifecycleStartup, LifecycleReload, LifecycleValidate} {
		log := logrus.New()
		log.SetLevel(logrus.InfoLevel)
		hook := &captureLogHook{}
		log.AddHook(hook)

		EmitStage(log, lc, StageReadConfig, 0, 0, 0, "")
		if len(hook.entries) != 1 {
			t.Fatalf("expected 1 entry for lifecycle %q", lc)
		}
		assert.Equal(t, lc, hook.entries[0].Data["lifecycle"])
	}
}

func TestEmitStage_AllStageNames(t *testing.T) {
	stages := []string{
		StageReadConfig,
		StageFakeIPAutoExpand,
		StageDaednsRouterBuild,
		StageDnsControllerBuild,
		StageMainRoutingOptimize,
		StageMainRoutingMatcher,
		StageControlPlaneBuild,
		StageReloadHandoff,
		StageReloadRetire,
		StageDaednsRequestProgramNormalize,
		StageDaednsUpstreamInit,
		StageDaednsRequestMatcherBuild,
		StageDaednsMatchersCompile,
	}
	for _, stage := range stages {
		log := logrus.New()
		log.SetLevel(logrus.InfoLevel)
		hook := &captureLogHook{}
		log.AddHook(hook)

		EmitStage(log, LifecycleStartup, stage, 100, 0, 0, "")
		if len(hook.entries) != 1 {
			t.Fatalf("expected 1 entry for stage %q", stage)
		}
		assert.Equal(t, stage, hook.entries[0].Data["stage"])
	}
}

func TestEmitStage_DurationGranularity(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitStage(log, LifecycleStartup, StageReadConfig, 0, 0, 0, "")
	assert.Equal(t, int64(0), hook.entries[0].Data["duration_ms"])

	EmitStage(log, LifecycleStartup, StageReadConfig, 123456789, 0, 0, "")
	assert.Equal(t, int64(123456789), hook.entries[1].Data["duration_ms"])
}

// TestEmitConfigStage_HasConfigCountFields verifies that a config-load child
// stage event carries all four count fields independent of duration, and
// that result=ok is set when errorClass is empty.
//
// Spec: each config child-stage event must be independently useful, so the
// count fields appear on every event (never omitted) and missing values are
// reported as 0.
func TestEmitConfigStage_HasConfigCountFields(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitConfigStage(log, LifecycleStartup, StageConfigParse, 12840, ConfigLoadFields{
		IncludedFiles:   14,
		ConfigBytes:     482301,
		ParsedSections:  51,
		RawRoutingRules: 2067,
	}, "")

	if len(hook.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.entries))
	}
	e := hook.entries[0]
	assert.Equal(t, "rules_load_stage", e.Data["event"])
	assert.Equal(t, "config_parse", e.Data["stage"])
	assert.Equal(t, int64(12840), e.Data["duration_ms"])
	assert.Equal(t, "ok", e.Data["result"])
	assert.Equal(t, 14, e.Data["included_files"])
	assert.Equal(t, int64(482301), e.Data["config_bytes"])
	assert.Equal(t, 51, e.Data["parsed_sections"])
	assert.Equal(t, 2067, e.Data["raw_routing_rules"])
}

// TestEmitConfigStage_ZeroCountsStillEmitted enforces the spec rule that
// unknown counts are reported as 0, not omitted.
func TestEmitConfigStage_ZeroCountsStillEmitted(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitConfigStage(log, LifecycleReload, StageConfigIncludeExpand, 18, ConfigLoadFields{}, "")

	if len(hook.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.entries))
	}
	e := hook.entries[0]
	assert.Equal(t, 0, e.Data["included_files"])
	assert.Equal(t, int64(0), e.Data["config_bytes"])
	assert.Equal(t, 0, e.Data["parsed_sections"])
	assert.Equal(t, 0, e.Data["raw_routing_rules"])
}

// TestEmitConfigStage_ErrorClassMarksResultError verifies that the error
// path lands result=error and the error_class token, alongside the count
// context for the stage that failed.
func TestEmitConfigStage_ErrorClassMarksResultError(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitConfigStage(log, LifecycleValidate, StageConfigParse, 8, ConfigLoadFields{
		IncludedFiles: 1,
		ConfigBytes:   42,
	}, "config_parse_error")

	if len(hook.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.entries))
	}
	e := hook.entries[0]
	assert.Equal(t, "error", e.Data["result"])
	assert.Equal(t, "config_parse_error", e.Data["error_class"])
	assert.Equal(t, 1, e.Data["included_files"])
}

// TestEmitConfigStage_AllConfigChildStageNames sanity-checks that every
// config-load stage constant produces a parseable event.
func TestEmitConfigStage_AllConfigChildStageNames(t *testing.T) {
	stages := []string{
		StageConfigReadFiles,
		StageConfigParse,
		StageConfigIncludeExpand,
		StageConfigMerge,
		StageConfigDecode,
		StageConfigPatch,
	}
	for _, stage := range stages {
		log := logrus.New()
		log.SetLevel(logrus.InfoLevel)
		hook := &captureLogHook{}
		log.AddHook(hook)

		EmitConfigStage(log, LifecycleStartup, stage, 1, ConfigLoadFields{}, "")
		if len(hook.entries) != 1 {
			t.Fatalf("expected 1 entry for stage %q", stage)
		}
		assert.Equal(t, stage, hook.entries[0].Data["stage"])
	}
}

// TestEmitSummary_HasConfigBreakdownFields verifies that every config child
// stage duration, the unattributed total, and the four config counts appear
// in the summary event.
func TestEmitSummary_HasConfigBreakdownFields(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitSummary(log, Summary{
		Lifecycle:             LifecycleReload,
		TotalMs:               16714,
		ConfigLoadMs:          13215,
		ConfigReadFilesMs:     12,
		ConfigParseMs:         12840,
		ConfigIncludeExpandMs: 18,
		ConfigMergeMs:         45,
		ConfigDecodeMs:        190,
		ConfigPatchMs:         75,
		ConfigUnattributedMs:  35,
		IncludedFiles:         14,
		ConfigBytes:           482301,
		ParsedSections:        51,
		RawRoutingRules:       2067,
	})

	if len(hook.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.entries))
	}
	e := hook.entries[0]
	assert.Equal(t, int64(13215), e.Data["config_load_ms"])
	assert.Equal(t, int64(12), e.Data["config_read_files_ms"])
	assert.Equal(t, int64(12840), e.Data["config_parse_ms"])
	assert.Equal(t, int64(18), e.Data["config_include_expand_ms"])
	assert.Equal(t, int64(45), e.Data["config_merge_ms"])
	assert.Equal(t, int64(190), e.Data["config_decode_ms"])
	assert.Equal(t, int64(75), e.Data["config_patch_ms"])
	assert.Equal(t, int64(35), e.Data["config_unattributed_ms"])
	assert.Equal(t, 14, e.Data["included_files"])
	assert.Equal(t, int64(482301), e.Data["config_bytes"])
	assert.Equal(t, 51, e.Data["parsed_sections"])
	assert.Equal(t, 2067, e.Data["raw_routing_rules"])
}

// TestEmitSummary_HasDaednsRouterBreakdownFields verifies the four
// daedns_router_build substage durations and the unattributed remainder appear
// in the summary event.
func TestEmitSummary_HasDaednsRouterBreakdownFields(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	hook := &captureLogHook{}
	log.AddHook(hook)

	EmitSummary(log, Summary{
		Lifecycle:                       LifecycleStartup,
		TotalMs:                         2616,
		DaednsRouterBuildMs:             763,
		DaednsRequestProgramNormalizeMs: 612,
		DaednsUpstreamInitMs:            5,
		DaednsRequestMatcherBuildMs:     110,
		DaednsMatchersCompileMs:         25,
		DaednsRouterUnattributedMs:      11,
	})

	if len(hook.entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(hook.entries))
	}
	e := hook.entries[0]
	assert.Equal(t, int64(763), e.Data["daedns_router_build_ms"])
	assert.Equal(t, int64(612), e.Data["daedns_request_program_normalize_ms"])
	assert.Equal(t, int64(5), e.Data["daedns_upstream_init_ms"])
	assert.Equal(t, int64(110), e.Data["daedns_request_matcher_build_ms"])
	assert.Equal(t, int64(25), e.Data["daedns_matchers_compile_ms"])
	assert.Equal(t, int64(11), e.Data["daedns_router_unattributed_ms"])
}
