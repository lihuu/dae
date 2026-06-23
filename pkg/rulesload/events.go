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
)

const (
	EventStage   = "rules_load_stage"
	EventSummary = "rules_load_summary"
)

// EmitStage logs a structured rules_load_stage event.
func EmitStage(log *logrus.Logger, lifecycle Lifecycle, stage string, durationMs int64, rulesIn, rulesOut int, errorClass string) {
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
	Lifecycle            Lifecycle
	TotalMs              int64
	ConfigLoadMs         int64
	FakeIPAutoExpandMs   int64
	DaednsRouterBuildMs  int64
	DnsControllerBuildMs int64
	MainRoutingOptimizeMs int64
	MainRoutingMatcherMs int64
	ControlPlaneBuildMs  int64
	ReloadHandoffMs      int64
	ReloadRetireMs       int64
	RulesTotal           int
	MainRoutingRules     int
	DnsRequestRules      int
	DnsResponseRules     int
	FakeIPAutoDerived    int
	ErrorClass           string
}

// EmitSummary logs a structured rules_load_summary event.
func EmitSummary(log *logrus.Logger, s Summary) {
	fields := logrus.Fields{
		"component":                 "rules_load",
		"event":                     EventSummary,
		"event_version":             1,
		"lifecycle":                 s.Lifecycle,
		"total_ms":                  s.TotalMs,
		"config_load_ms":            s.ConfigLoadMs,
		"fakeip_auto_expand_ms":     s.FakeIPAutoExpandMs,
		"daedns_router_build_ms":    s.DaednsRouterBuildMs,
		"dns_controller_build_ms":   s.DnsControllerBuildMs,
		"main_routing_optimize_ms":  s.MainRoutingOptimizeMs,
		"main_routing_matcher_build_ms": s.MainRoutingMatcherMs,
		"control_plane_build_ms":    s.ControlPlaneBuildMs,
		"reload_handoff_ms":         s.ReloadHandoffMs,
		"reload_retire_ms":          s.ReloadRetireMs,
		"rules_total":               s.RulesTotal,
		"main_routing_rules":        s.MainRoutingRules,
		"dns_request_rules":         s.DnsRequestRules,
		"dns_response_rules":        s.DnsResponseRules,
		"fakeip_auto_derived_rules": s.FakeIPAutoDerived,
	}
	if s.ErrorClass != "" {
		fields["result"] = "error"
		fields["error_class"] = s.ErrorClass
	} else {
		fields["result"] = "ok"
	}
	log.WithFields(fields).Infoln("Rules load summary")
}