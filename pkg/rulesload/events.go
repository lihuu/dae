// Package rulesload provides structured observability events for DAE rule loading,
// config loading, and routing-rule generation. All events use stable logrus field
// keys so operators and tools can machine-parse them independent of the log format.
package rulesload

import "github.com/sirupsen/logrus"

// Lifecycle represents the type of rules load operation.
type Lifecycle string

const (
	LifecycleStartup  Lifecycle = "startup"
	LifecycleReload   Lifecycle = "reload"
	LifecycleValidate Lifecycle = "validate"
)

// Stage names — every value emitted as `stage` in rules_load_stage events.
const (
	StageReadConfig          = "read_config"
	StageFakeIPAutoExpand    = "fakeip_auto_expand"
	StageDaednsRouterBuild   = "daedns_router_build"
	StageDnsControllerBuild  = "dns_controller_build"
	StageMainRoutingOptimize = "main_routing_optimize"
	StageMainRoutingMatcher  = "main_routing_matcher_build"
	StageControlPlaneBuild   = "control_plane_build"
	StageReloadHandoff       = "reload_handoff"
	StageReloadRetire        = "reload_retire"

	// Config-load child stages (spec
	// docs/superpowers/specs/2026-06-25-dae-config-load-breakdown-observability-design.md).
	// These break down read_config into machine-parseable child stages so an
	// operator can identify which step inside readConfig is the dominant cost.
	StageConfigReadFiles     = "config_read_files"
	StageConfigParse         = "config_parse"
	StageConfigIncludeExpand = "config_include_expand"
	StageConfigMerge         = "config_merge"
	StageConfigDecode        = "config_decode"
	StageConfigPatch         = "config_patch"

	// daedns_router_build child stages — decompose the otherwise-opaque
	// daedns_router_build_ms parent stage into the four substages observed
	// inside component/daedns.NewWithOption. Emitted by cmd/run.go after
	// receiving a daedns.BuildStats from NewOption.Stats.
	StageDaednsRequestProgramNormalize = "daedns_request_program_normalize"
	StageDaednsUpstreamInit            = "daedns_upstream_init"
	StageDaednsRequestMatcherBuild     = "daedns_request_matcher_build"
	StageDaednsMatchersCompile         = "daedns_matchers_compile"
)

const (
	EventStage   = "rules_load_stage"
	EventSummary = "rules_load_summary"
)

// ConfigLoadFields carries the per-event counter context for config-load
// child stages. These four fields appear on EVERY config child-stage event
// (spec: "each event is independently useful") so operators can correlate
// duration with size without joining against the summary.
//
// Zero values are emitted verbatim: the spec mandates that unknown/not-yet-
// available counts are 0 rather than omitted.
type ConfigLoadFields struct {
	IncludedFiles   int
	ConfigBytes     int64
	ParsedSections  int
	RawRoutingRules int
}

// EmitStage logs a structured rules_load_stage event without config-load
// counter context. Existing callers (FakeIP expand, main routing optimize,
// dns_controller_build, etc.) keep using this signature.
func EmitStage(log *logrus.Logger, lifecycle Lifecycle, stage string, durationMs int64, rulesIn, rulesOut int, errorClass string) {
	emitStageInternal(log, lifecycle, stage, durationMs, rulesIn, rulesOut, errorClass, nil)
}

// EmitConfigStage logs a rules_load_stage event for a config-load child
// stage. The four count fields land in the event alongside `duration_ms`,
// `rules_in`, and `rules_out` so each stage line is independently useful.
//
// For successful stages errorClass is "". For failed stages the spec
// constrains the token: one of the StageConfig* names suffixed with `_error`,
// e.g. `config_parse_error`. Callers in the cmd layer construct the token.
func EmitConfigStage(
	log *logrus.Logger,
	lifecycle Lifecycle,
	stage string,
	durationMs int64,
	counts ConfigLoadFields,
	errorClass string,
) {
	emitStageInternal(log, lifecycle, stage, durationMs, 0, 0, errorClass, &counts)
}

func emitStageInternal(
	log *logrus.Logger,
	lifecycle Lifecycle,
	stage string,
	durationMs int64,
	rulesIn, rulesOut int,
	errorClass string,
	counts *ConfigLoadFields,
) {
	fields := logrus.Fields{
		"component":     "rules_load",
		"event":         EventStage,
		"event_version": 1,
		"lifecycle":     lifecycle,
		"stage":         stage,
		"duration_ms":   durationMs,
		"rules_in":      rulesIn,
		"rules_out":     rulesOut,
	}
	if counts != nil {
		// Always emit all four config-load count fields when the stage is a
		// config-load child stage; missing data is reported as 0.
		fields["included_files"] = counts.IncludedFiles
		fields["config_bytes"] = counts.ConfigBytes
		fields["parsed_sections"] = counts.ParsedSections
		fields["raw_routing_rules"] = counts.RawRoutingRules
	}
	if errorClass != "" {
		fields["result"] = "error"
		fields["error_class"] = errorClass
	} else {
		fields["result"] = "ok"
	}
	log.WithFields(fields).Infoln("Rules load stage: " + stage)
}

// Summary carries all accumulated timing and count data for a single load lifecycle.
type Summary struct {
	Lifecycle             Lifecycle
	TotalMs               int64
	ConfigLoadMs          int64
	FakeIPAutoExpandMs    int64
	DaednsRouterBuildMs   int64
	DnsControllerBuildMs  int64
	MainRoutingOptimizeMs int64
	MainRoutingMatcherMs  int64
	ControlPlaneBuildMs   int64
	ReloadHandoffMs       int64
	ReloadRetireMs        int64

	// Config-load child stages.
	ConfigReadFilesMs     int64
	ConfigParseMs         int64
	ConfigIncludeExpandMs int64
	ConfigMergeMs         int64
	ConfigDecodeMs        int64
	ConfigPatchMs         int64
	ConfigUnattributedMs  int64

	// daedns_router_build child stages.
	DaednsRequestProgramNormalizeMs int64
	DaednsUpstreamInitMs            int64
	DaednsRequestMatcherBuildMs     int64
	DaednsMatchersCompileMs         int64
	DaednsRouterUnattributedMs      int64

	// Config-load counters.
	IncludedFiles   int
	ConfigBytes     int64
	ParsedSections  int
	RawRoutingRules int

	RulesTotal        int
	MainRoutingRules  int
	DnsRequestRules   int
	DnsResponseRules  int
	FakeIPAutoDerived int
	ErrorClass        string
}

// EmitSummary logs a structured rules_load_summary event.
func EmitSummary(log *logrus.Logger, s Summary) {
	fields := logrus.Fields{
		"component":                     "rules_load",
		"event":                         EventSummary,
		"event_version":                 1,
		"lifecycle":                     s.Lifecycle,
		"total_ms":                      s.TotalMs,
		"config_load_ms":                s.ConfigLoadMs,
		"fakeip_auto_expand_ms":         s.FakeIPAutoExpandMs,
		"daedns_router_build_ms":        s.DaednsRouterBuildMs,
		"dns_controller_build_ms":       s.DnsControllerBuildMs,
		"main_routing_optimize_ms":      s.MainRoutingOptimizeMs,
		"main_routing_matcher_build_ms": s.MainRoutingMatcherMs,
		"control_plane_build_ms":        s.ControlPlaneBuildMs,
		"reload_handoff_ms":             s.ReloadHandoffMs,
		"reload_retire_ms":              s.ReloadRetireMs,
		"config_read_files_ms":          s.ConfigReadFilesMs,
		"config_parse_ms":               s.ConfigParseMs,
		"config_include_expand_ms":      s.ConfigIncludeExpandMs,
		"config_merge_ms":               s.ConfigMergeMs,
		"config_decode_ms":              s.ConfigDecodeMs,
		"config_patch_ms":               s.ConfigPatchMs,
		"config_unattributed_ms":        s.ConfigUnattributedMs,
		"daedns_request_program_normalize_ms": s.DaednsRequestProgramNormalizeMs,
		"daedns_upstream_init_ms":             s.DaednsUpstreamInitMs,
		"daedns_request_matcher_build_ms":     s.DaednsRequestMatcherBuildMs,
		"daedns_matchers_compile_ms":          s.DaednsMatchersCompileMs,
		"daedns_router_unattributed_ms":       s.DaednsRouterUnattributedMs,
		"included_files":                s.IncludedFiles,
		"config_bytes":                  s.ConfigBytes,
		"parsed_sections":               s.ParsedSections,
		"raw_routing_rules":             s.RawRoutingRules,
		"rules_total":                   s.RulesTotal,
		"main_routing_rules":            s.MainRoutingRules,
		"dns_request_rules":             s.DnsRequestRules,
		"dns_response_rules":            s.DnsResponseRules,
		"fakeip_auto_derived_rules":     s.FakeIPAutoDerived,
	}
	if s.ErrorClass != "" {
		fields["result"] = "error"
		fields["error_class"] = s.ErrorClass
	} else {
		fields["result"] = "ok"
	}
	log.WithFields(fields).Infoln("Rules load summary")
}
