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
		Lifecycle:   LifecycleValidate,
		ErrorClass:  "config_parse_error",
		TotalMs:     500,
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