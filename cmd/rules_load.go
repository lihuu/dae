package cmd

import (
	"sync"
	"time"

	"github.com/daeuniverse/dae/component/daedns"
	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/pkg/rulesload"
	"github.com/sirupsen/logrus"
)

// SummaryCollector receives stage notifications during control plane construction
// and accumulates a final rulesload.Summary that is emitted as a structured log event.
//
// Create one per lifecycle (startup, reload, validate) via newSummaryCollector,
// wire it into newControlPlaneWithMode, and call emit() when the full build sequence
// completes.
type SummaryCollector struct {
	mu sync.Mutex

	log       *logrus.Logger
	lifecycle rulesload.Lifecycle

	totalStart    time.Time
	configLoadDur time.Duration

	// Accumulated from observer calls.
	fakeipAutoExpand    time.Duration
	daednsRouterBuild   time.Duration
	dnsControllerBuild  time.Duration
	mainRoutingOptimize time.Duration
	mainRoutingMatcher  time.Duration
	controlPlaneBuild   time.Duration
	reloadHandoff       time.Duration
	reloadRetire        time.Duration

	// Config-load child stages (spec 2026-06-25).
	configReadFiles     time.Duration
	configParse         time.Duration
	configIncludeExpand time.Duration
	configMerge         time.Duration
	configDecode        time.Duration
	configPatch         time.Duration

	// daedns_router_build child stages — fed from component/daedns.BuildStats
	// via EmitDaednsRouterStages. Their sum is expected to be ≈
	// daednsRouterBuild; any gap becomes daedns_router_unattributed_ms in the
	// summary snapshot.
	daednsRequestProgramNormalize time.Duration
	daednsUpstreamInit            time.Duration
	daednsRequestMatcherBuild     time.Duration
	daednsMatchersCompile         time.Duration

	// Config-load counters.
	includedFiles   int
	configBytes     int64
	parsedSections  int
	rawRoutingRules int

	// Counts (set explicitly where known).
	mainRoutingRules  int
	dnsRequestRules   int
	dnsResponseRules  int
	fakeipAutoDerived int

	errorClass string
}

func newSummaryCollector(log *logrus.Logger, lifecycle rulesload.Lifecycle) *SummaryCollector {
	return &SummaryCollector{
		log:        log,
		lifecycle:  lifecycle,
		totalStart: time.Now(),
	}
}

// RecordConfigLoad records the duration of readConfig.
func (c *SummaryCollector) RecordConfigLoad(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configLoadDur = d
}

// RecordConfigStats records the per-stage durations and counters produced by
// the stats-aware config loader. It is additive: callers can invoke it twice
// (e.g. once for MergeWithStats result and once for NewWithStats result) and
// the collector aggregates everything into the final summary.
//
// The function does NOT emit any structured event by itself; the caller is
// responsible for emitting per-stage rules_load_stage events via
// EmitConfigStageOK / EmitConfigStageError. Splitting record vs emit keeps the
// emit path explicit for AC-05 / AC-07 and lets the validate path suppress
// stage events when --timings is off.
func (c *SummaryCollector) RecordConfigStats(stats config.LoadStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configReadFiles += stats.ReadFilesDuration
	c.configParse += stats.ParseDuration
	c.configIncludeExpand += stats.IncludeExpandDuration
	c.configMerge += stats.MergeDuration
	c.configDecode += stats.DecodeDuration
	c.configPatch += stats.PatchDuration
	if stats.IncludedFiles != 0 {
		c.includedFiles = stats.IncludedFiles
	}
	if stats.ConfigBytes != 0 {
		c.configBytes = stats.ConfigBytes
	}
	if stats.ParsedSections != 0 {
		c.parsedSections = stats.ParsedSections
	}
	if stats.RawRoutingRules != 0 {
		c.rawRoutingRules = stats.RawRoutingRules
	}
}

// configCountFieldsLocked snapshots the current config-load counters as a
// rulesload.ConfigLoadFields. Caller must hold c.mu.
func (c *SummaryCollector) configCountFieldsLocked() rulesload.ConfigLoadFields {
	return rulesload.ConfigLoadFields{
		IncludedFiles:   c.includedFiles,
		ConfigBytes:     c.configBytes,
		ParsedSections:  c.parsedSections,
		RawRoutingRules: c.rawRoutingRules,
	}
}

// EmitConfigStages emits one successful rules_load_stage event per completed
// config child stage in stats. The event carries the current accumulated
// config-load counts (the spec guarantees they are present on every event).
//
// Skipped stages (zero duration AND no failure) emit nothing, so an opt-in
// validate path that didn't actually run patches won't fabricate an event.
//
// This MUST be called AFTER RecordConfigStats so the count fields reflect the
// latest stats values.
func (c *SummaryCollector) EmitConfigStages(stats config.LoadStats) {
	c.mu.Lock()
	log := c.log
	lifecycle := c.lifecycle
	counts := c.configCountFieldsLocked()
	c.mu.Unlock()

	failed := stats.FailedStage
	emit := func(stage string, dur time.Duration) {
		// Skip stages we never reached: zero duration AND not the failing
		// stage. The failing stage emits even at duration 0 so operators see
		// the failure.
		if dur == 0 && stage != failed {
			return
		}
		errorClass := ""
		if stage == failed {
			errorClass = stage + "_error"
		}
		rulesload.EmitConfigStage(log, lifecycle, stage, dur.Milliseconds(), counts, errorClass)
	}
	emit(rulesload.StageConfigReadFiles, stats.ReadFilesDuration)
	emit(rulesload.StageConfigParse, stats.ParseDuration)
	emit(rulesload.StageConfigIncludeExpand, stats.IncludeExpandDuration)
	emit(rulesload.StageConfigMerge, stats.MergeDuration)
	emit(rulesload.StageConfigDecode, stats.DecodeDuration)
	emit(rulesload.StageConfigPatch, stats.PatchDuration)
}

// EmitDaednsRouterStages records and emits the four daedns_router_build child
// stages. Like EmitConfigStages, it both accumulates the durations into the
// summary AND publishes one rules_load_stage event per stage on the
// collector's logger using the collector's lifecycle. Skipped stages (zero
// duration) emit nothing.
//
// MUST be called AFTER the parent StageDaednsRouterBuild has been emitted so
// operators see the parent stage before its children.
func (c *SummaryCollector) EmitDaednsRouterStages(stats daedns.BuildStats) {
	c.mu.Lock()
	c.daednsRequestProgramNormalize += stats.RequestProgramNormalize
	c.daednsUpstreamInit += stats.UpstreamInit
	c.daednsRequestMatcherBuild += stats.RequestMatcherBuild
	c.daednsMatchersCompile += stats.MatchersCompile
	log := c.log
	lifecycle := c.lifecycle
	c.mu.Unlock()

	emit := func(stage string, dur time.Duration) {
		if dur == 0 {
			return
		}
		rulesload.EmitStage(log, lifecycle, stage, dur.Milliseconds(), 0, 0, "")
	}
	emit(rulesload.StageDaednsRequestProgramNormalize, stats.RequestProgramNormalize)
	emit(rulesload.StageDaednsUpstreamInit, stats.UpstreamInit)
	emit(rulesload.StageDaednsRequestMatcherBuild, stats.RequestMatcherBuild)
	emit(rulesload.StageDaednsMatchersCompile, stats.MatchersCompile)
}

// RecordStage implements rulesload.Observer by accumulating per-stage durations.
// This is safe for concurrent calls from inside the control plane constructor.
func (c *SummaryCollector) RecordStage(stage string, durationMs int64, rulesIn, rulesOut int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.recordStageLocked(stage, durationMs, rulesOut)
}

// EmitStage implements rulesload.Observer by accumulating the stage into the
// summary AND publishing a standalone rules_load_stage event on the collector's
// logger using the collector's lifecycle. errorClass != "" marks the event
// result=error.
//
// This is the path used by control_plane.go build sites that don't carry a
// *logrus.Logger or lifecycle through their own arguments; the collector
// supplies both, keeping the build code observation-agnostic.
func (c *SummaryCollector) EmitStage(stage string, durationMs int64, rulesIn, rulesOut int, errorClass string) {
	c.mu.Lock()
	c.recordStageLocked(stage, durationMs, rulesOut)
	log := c.log
	lifecycle := c.lifecycle
	c.mu.Unlock()

	rulesload.EmitStage(log, lifecycle, stage, durationMs, rulesIn, rulesOut, errorClass)
}

// recordStageLocked is the bookkeeping half of RecordStage / EmitStage. The
// caller must hold c.mu.
func (c *SummaryCollector) recordStageLocked(stage string, durationMs int64, rulesOut int) {
	d := time.Duration(durationMs) * time.Millisecond
	switch stage {
	case rulesload.StageFakeIPAutoExpand:
		c.fakeipAutoExpand = d
	case rulesload.StageDaednsRouterBuild:
		c.daednsRouterBuild = d
	case rulesload.StageDnsControllerBuild:
		c.dnsControllerBuild = d
	case rulesload.StageMainRoutingOptimize:
		c.mainRoutingOptimize = d
		c.mainRoutingRules = rulesOut
	case rulesload.StageMainRoutingMatcher:
		c.mainRoutingMatcher = d
	case rulesload.StageControlPlaneBuild:
		c.controlPlaneBuild = d
	case rulesload.StageReloadHandoff:
		c.reloadHandoff = d
	case rulesload.StageReloadRetire:
		c.reloadRetire = d
	case rulesload.StageDaednsRequestProgramNormalize:
		c.daednsRequestProgramNormalize = d
	case rulesload.StageDaednsUpstreamInit:
		c.daednsUpstreamInit = d
	case rulesload.StageDaednsRequestMatcherBuild:
		c.daednsRequestMatcherBuild = d
	case rulesload.StageDaednsMatchersCompile:
		c.daednsMatchersCompile = d
	}
}

// SetFakeIPAutoDerived records the number of rules derived by FakeIP expansion.
func (c *SummaryCollector) SetFakeIPAutoDerived(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fakeipAutoDerived = n
}

// SetDnsRouting records the DNS routing rule counts (request and response).
// Counts are taken after FakeIP Auto expansion so the request count reflects
// the rules that will be compiled into the DNS request matcher.
func (c *SummaryCollector) SetDnsRouting(request, response int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dnsRequestRules = request
	c.dnsResponseRules = response
}

// SetError records an error class for the summary.
func (c *SummaryCollector) SetError(errorClass string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errorClass = errorClass
}

// Emit publishes the accumulated summary as a structured logrus event.
func (c *SummaryCollector) Emit() {
	c.mu.Lock()
	s := c.buildSnapshot()
	c.mu.Unlock()

	rulesload.EmitSummary(c.log, s)
}

func (c *SummaryCollector) buildSnapshot() rulesload.Summary {
	total := time.Since(c.totalStart)
	configLoadMs := c.configLoadDur.Milliseconds()
	configReadFilesMs := c.configReadFiles.Milliseconds()
	configParseMs := c.configParse.Milliseconds()
	configIncludeExpandMs := c.configIncludeExpand.Milliseconds()
	configMergeMs := c.configMerge.Milliseconds()
	configDecodeMs := c.configDecode.Milliseconds()
	configPatchMs := c.configPatch.Milliseconds()
	// Spec: config_unattributed_ms = max(0, config_load_ms - sum(child stages)).
	childSum := configReadFilesMs + configParseMs + configIncludeExpandMs +
		configMergeMs + configDecodeMs + configPatchMs
	configUnattributedMs := configLoadMs - childSum
	if configUnattributedMs < 0 {
		configUnattributedMs = 0
	}

	daednsRouterBuildMs := c.daednsRouterBuild.Milliseconds()
	daednsReqProgMs := c.daednsRequestProgramNormalize.Milliseconds()
	daednsUpstreamMs := c.daednsUpstreamInit.Milliseconds()
	daednsReqMatcherMs := c.daednsRequestMatcherBuild.Milliseconds()
	daednsMatchersMs := c.daednsMatchersCompile.Milliseconds()
	daednsChildSum := daednsReqProgMs + daednsUpstreamMs + daednsReqMatcherMs + daednsMatchersMs
	daednsRouterUnattributedMs := daednsRouterBuildMs - daednsChildSum
	if daednsRouterUnattributedMs < 0 {
		daednsRouterUnattributedMs = 0
	}

	return rulesload.Summary{
		Lifecycle:                       c.lifecycle,
		TotalMs:                         total.Milliseconds(),
		ConfigLoadMs:                    configLoadMs,
		FakeIPAutoExpandMs:              c.fakeipAutoExpand.Milliseconds(),
		DaednsRouterBuildMs:             daednsRouterBuildMs,
		DnsControllerBuildMs:            c.dnsControllerBuild.Milliseconds(),
		MainRoutingOptimizeMs:           c.mainRoutingOptimize.Milliseconds(),
		MainRoutingMatcherMs:            c.mainRoutingMatcher.Milliseconds(),
		ControlPlaneBuildMs:             c.controlPlaneBuild.Milliseconds(),
		ReloadHandoffMs:                 c.reloadHandoff.Milliseconds(),
		ReloadRetireMs:                  c.reloadRetire.Milliseconds(),
		ConfigReadFilesMs:               configReadFilesMs,
		ConfigParseMs:                   configParseMs,
		ConfigIncludeExpandMs:           configIncludeExpandMs,
		ConfigMergeMs:                   configMergeMs,
		ConfigDecodeMs:                  configDecodeMs,
		ConfigPatchMs:                   configPatchMs,
		ConfigUnattributedMs:            configUnattributedMs,
		DaednsRequestProgramNormalizeMs: daednsReqProgMs,
		DaednsUpstreamInitMs:            daednsUpstreamMs,
		DaednsRequestMatcherBuildMs:     daednsReqMatcherMs,
		DaednsMatchersCompileMs:         daednsMatchersMs,
		DaednsRouterUnattributedMs:      daednsRouterUnattributedMs,
		IncludedFiles:                   c.includedFiles,
		ConfigBytes:                     c.configBytes,
		ParsedSections:                  c.parsedSections,
		RawRoutingRules:                 c.rawRoutingRules,
		MainRoutingRules:                c.mainRoutingRules,
		DnsRequestRules:                 c.dnsRequestRules,
		DnsResponseRules:                c.dnsResponseRules,
		FakeIPAutoDerived:               c.fakeipAutoDerived,
		RulesTotal:                      c.mainRoutingRules + c.dnsRequestRules + c.dnsResponseRules + c.fakeipAutoDerived,
		ErrorClass:                      c.errorClass,
	}
}
