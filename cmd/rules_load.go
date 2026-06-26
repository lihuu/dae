package cmd

import (
	"sync"
	"time"

	"github.com/daeuniverse/dae/component/daedns"
	componentdns "github.com/daeuniverse/dae/component/dns"
	"github.com/daeuniverse/dae/component/routing/domain_matcher"
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

	// daedns_request_matcher_build sub-substages.
	daednsRequestMatcherLower   time.Duration
	daednsRequestMatcherCompile time.Duration

	// dns_controller_build child stages — fed from component/dns.BuildStats
	// via EmitDnsControllerStages. Their sum is expected to be ≈
	// dnsControllerBuild; any gap becomes dns_controller_unattributed_ms in
	// the summary snapshot.
	dnsUpstreamInit             time.Duration
	dnsRequestProgramNormalize  time.Duration
	dnsRequestMatcherLower      time.Duration
	dnsRequestMatcherCompile    time.Duration
	dnsResponseProgramNormalize time.Duration
	dnsResponseMatcherLower     time.Duration
	dnsResponseMatcherCompile   time.Duration

	// daedns_request_matcher_compile_distribution fields — per-slot breakdown
	// of the heavy AhocorasickSlimtrie.Build inside the daedns router.
	daednsRequestMatcherAcSlots             int
	daednsRequestMatcherAcPatterns          int
	daednsRequestMatcherAcMaxSlotPatterns   int
	daednsRequestMatcherAcCpuMs             int64
	daednsRequestMatcherAcMaxSlotMs         int64
	daednsRequestMatcherTrieSlots           int
	daednsRequestMatcherTriePatterns        int
	daednsRequestMatcherTrieMaxSlotPatterns int
	daednsRequestMatcherTrieCpuMs           int64
	daednsRequestMatcherTrieMaxSlotMs       int64
	daednsRequestMatcherRegexpSlots         int
	daednsRequestMatcherWallMs              int64

	// main_routing_matcher_compile_distribution fields — per-slot breakdown
	// of the heavy AhocorasickSlimtrie.Build inside the main routing matcher.
	mainRoutingMatcherAcSlots             int
	mainRoutingMatcherAcPatterns          int
	mainRoutingMatcherAcMaxSlotPatterns   int
	mainRoutingMatcherAcCpuMs             int64
	mainRoutingMatcherAcMaxSlotMs         int64
	mainRoutingMatcherTrieSlots           int
	mainRoutingMatcherTriePatterns        int
	mainRoutingMatcherTrieMaxSlotPatterns int
	mainRoutingMatcherTrieCpuMs           int64
	mainRoutingMatcherTrieMaxSlotMs       int64
	mainRoutingMatcherRegexpSlots         int
	mainRoutingMatcherWallMs              int64

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
// stages plus the two daedns_request_matcher_build sub-substages. Like
// EmitConfigStages, it both accumulates the durations into the summary AND
// publishes one rules_load_stage event per non-zero stage on the collector's
// logger using the collector's lifecycle.
//
// MUST be called AFTER the parent StageDaednsRouterBuild has been emitted so
// operators see the parent stage before its children.
func (c *SummaryCollector) EmitDaednsRouterStages(stats daedns.BuildStats) {
	c.mu.Lock()
	c.daednsRequestProgramNormalize += stats.RequestProgramNormalize
	c.daednsUpstreamInit += stats.UpstreamInit
	c.daednsRequestMatcherBuild += stats.RequestMatcherBuild
	c.daednsMatchersCompile += stats.MatchersCompile
	c.daednsRequestMatcherLower += stats.RequestMatcherLower
	c.daednsRequestMatcherCompile += stats.RequestMatcherCompile
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
	emit(rulesload.StageDaednsRequestMatcherLower, stats.RequestMatcherLower)
	emit(rulesload.StageDaednsRequestMatcherCompile, stats.RequestMatcherCompile)
	emit(rulesload.StageDaednsMatchersCompile, stats.MatchersCompile)
}

// EmitDnsControllerStages records and emits the seven dns_controller_build
// child stages. It both accumulates the durations into the summary AND
// publishes one rules_load_stage event per non-zero stage on the collector's
// logger using the collector's lifecycle.
//
// MUST be called AFTER the parent StageDnsControllerBuild has been emitted.
func (c *SummaryCollector) EmitDnsControllerStages(stats componentdns.BuildStats) {
	c.mu.Lock()
	c.dnsUpstreamInit += stats.UpstreamInit
	c.dnsRequestProgramNormalize += stats.RequestProgramNormalize
	c.dnsRequestMatcherLower += stats.RequestMatcherLower
	c.dnsRequestMatcherCompile += stats.RequestMatcherCompile
	c.dnsResponseProgramNormalize += stats.ResponseProgramNormalize
	c.dnsResponseMatcherLower += stats.ResponseMatcherLower
	c.dnsResponseMatcherCompile += stats.ResponseMatcherCompile
	log := c.log
	lifecycle := c.lifecycle
	c.mu.Unlock()

	emit := func(stage string, dur time.Duration) {
		if dur == 0 {
			return
		}
		rulesload.EmitStage(log, lifecycle, stage, dur.Milliseconds(), 0, 0, "")
	}
	emit(rulesload.StageDnsUpstreamInit, stats.UpstreamInit)
	emit(rulesload.StageDnsRequestProgramNormalize, stats.RequestProgramNormalize)
	emit(rulesload.StageDnsRequestMatcherLower, stats.RequestMatcherLower)
	emit(rulesload.StageDnsRequestMatcherCompile, stats.RequestMatcherCompile)
	emit(rulesload.StageDnsResponseProgramNormalize, stats.ResponseProgramNormalize)
	emit(rulesload.StageDnsResponseMatcherLower, stats.ResponseMatcherLower)
	emit(rulesload.StageDnsResponseMatcherCompile, stats.ResponseMatcherCompile)
}

// EmitDaednsRequestMatcherDistribution records and emits the per-slot
// breakdown of the heavy AhocorasickSlimtrie.Build inside the daedns router.
// It both accumulates the distribution into the summary AND publishes one
// rules_load_stage event with all twelve detail fields on the collector's
// logger using the collector's lifecycle.
//
// MUST be called AFTER the parent StageDaednsRequestMatcherCompile has been
// emitted so operators see the parent stage before its distribution.
func (c *SummaryCollector) EmitDaednsRequestMatcherDistribution(stats *domain_matcher.BuildStats) {
	if stats == nil {
		return
	}
	c.mu.Lock()
	c.daednsRequestMatcherAcSlots = stats.AcSlots
	c.daednsRequestMatcherAcPatterns = stats.AcPatterns
	c.daednsRequestMatcherAcMaxSlotPatterns = stats.AcMaxSlotPatterns
	c.daednsRequestMatcherAcCpuMs = stats.AcCpuDuration.Milliseconds()
	c.daednsRequestMatcherAcMaxSlotMs = stats.AcMaxSlotDuration.Milliseconds()
	c.daednsRequestMatcherTrieSlots = stats.TrieSlots
	c.daednsRequestMatcherTriePatterns = stats.TriePatterns
	c.daednsRequestMatcherTrieMaxSlotPatterns = stats.TrieMaxSlotPatterns
	c.daednsRequestMatcherTrieCpuMs = stats.TrieCpuDuration.Milliseconds()
	c.daednsRequestMatcherTrieMaxSlotMs = stats.TrieMaxSlotDuration.Milliseconds()
	c.daednsRequestMatcherRegexpSlots = stats.RegexpSlots
	c.daednsRequestMatcherWallMs = stats.WallDuration.Milliseconds()
	log := c.log
	lifecycle := c.lifecycle
	c.mu.Unlock()

	rulesload.EmitMatcherDistribution(log, lifecycle, rulesload.StageDaednsRequestMatcherCompileDistribution, rulesload.MatcherDistributionFields{
		AcSlots:             stats.AcSlots,
		AcPatterns:          stats.AcPatterns,
		AcMaxSlotPatterns:   stats.AcMaxSlotPatterns,
		AcCpuMs:             stats.AcCpuDuration.Milliseconds(),
		AcMaxSlotMs:         stats.AcMaxSlotDuration.Milliseconds(),
		TrieSlots:           stats.TrieSlots,
		TriePatterns:        stats.TriePatterns,
		TrieMaxSlotPatterns: stats.TrieMaxSlotPatterns,
		TrieCpuMs:           stats.TrieCpuDuration.Milliseconds(),
		TrieMaxSlotMs:       stats.TrieMaxSlotDuration.Milliseconds(),
		RegexpSlots:         stats.RegexpSlots,
		WallMs:              stats.WallDuration.Milliseconds(),
	})
}

// EmitMainRoutingMatcherDistribution records and emits the per-slot breakdown
// of the heavy AhocorasickSlimtrie.Build inside the main routing matcher. It
// both accumulates the distribution into the summary AND publishes one
// rules_load_stage event with all twelve detail fields on the collector's
// logger using the collector's lifecycle.
//
// MUST be called AFTER the parent StageMainRoutingMatcher has been emitted so
// operators see the parent stage before its distribution.
func (c *SummaryCollector) EmitMainRoutingMatcherDistribution(stats *domain_matcher.BuildStats) {
	if stats == nil {
		return
	}
	c.mu.Lock()
	c.mainRoutingMatcherAcSlots = stats.AcSlots
	c.mainRoutingMatcherAcPatterns = stats.AcPatterns
	c.mainRoutingMatcherAcMaxSlotPatterns = stats.AcMaxSlotPatterns
	c.mainRoutingMatcherAcCpuMs = stats.AcCpuDuration.Milliseconds()
	c.mainRoutingMatcherAcMaxSlotMs = stats.AcMaxSlotDuration.Milliseconds()
	c.mainRoutingMatcherTrieSlots = stats.TrieSlots
	c.mainRoutingMatcherTriePatterns = stats.TriePatterns
	c.mainRoutingMatcherTrieMaxSlotPatterns = stats.TrieMaxSlotPatterns
	c.mainRoutingMatcherTrieCpuMs = stats.TrieCpuDuration.Milliseconds()
	c.mainRoutingMatcherTrieMaxSlotMs = stats.TrieMaxSlotDuration.Milliseconds()
	c.mainRoutingMatcherRegexpSlots = stats.RegexpSlots
	c.mainRoutingMatcherWallMs = stats.WallDuration.Milliseconds()
	log := c.log
	lifecycle := c.lifecycle
	c.mu.Unlock()

	rulesload.EmitMatcherDistribution(log, lifecycle, rulesload.StageMainRoutingMatcherCompileDistribution, rulesload.MatcherDistributionFields{
		AcSlots:             stats.AcSlots,
		AcPatterns:          stats.AcPatterns,
		AcMaxSlotPatterns:   stats.AcMaxSlotPatterns,
		AcCpuMs:             stats.AcCpuDuration.Milliseconds(),
		AcMaxSlotMs:         stats.AcMaxSlotDuration.Milliseconds(),
		TrieSlots:           stats.TrieSlots,
		TriePatterns:        stats.TriePatterns,
		TrieMaxSlotPatterns: stats.TrieMaxSlotPatterns,
		TrieCpuMs:           stats.TrieCpuDuration.Milliseconds(),
		TrieMaxSlotMs:       stats.TrieMaxSlotDuration.Milliseconds(),
		RegexpSlots:         stats.RegexpSlots,
		WallMs:              stats.WallDuration.Milliseconds(),
	})
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
	case rulesload.StageDaednsRequestMatcherLower:
		c.daednsRequestMatcherLower = d
	case rulesload.StageDaednsRequestMatcherCompile:
		c.daednsRequestMatcherCompile = d
	case rulesload.StageDnsUpstreamInit:
		c.dnsUpstreamInit = d
	case rulesload.StageDnsRequestProgramNormalize:
		c.dnsRequestProgramNormalize = d
	case rulesload.StageDnsRequestMatcherLower:
		c.dnsRequestMatcherLower = d
	case rulesload.StageDnsRequestMatcherCompile:
		c.dnsRequestMatcherCompile = d
	case rulesload.StageDnsResponseProgramNormalize:
		c.dnsResponseProgramNormalize = d
	case rulesload.StageDnsResponseMatcherLower:
		c.dnsResponseMatcherLower = d
	case rulesload.StageDnsResponseMatcherCompile:
		c.dnsResponseMatcherCompile = d
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

	daednsReqMatcherLowerMs := c.daednsRequestMatcherLower.Milliseconds()
	daednsReqMatcherCompileMs := c.daednsRequestMatcherCompile.Milliseconds()

	dnsControllerBuildMs := c.dnsControllerBuild.Milliseconds()
	dnsUpstreamInitMs := c.dnsUpstreamInit.Milliseconds()
	dnsReqProgMs := c.dnsRequestProgramNormalize.Milliseconds()
	dnsReqMatcherLowerMs := c.dnsRequestMatcherLower.Milliseconds()
	dnsReqMatcherCompileMs := c.dnsRequestMatcherCompile.Milliseconds()
	dnsRespProgMs := c.dnsResponseProgramNormalize.Milliseconds()
	dnsRespMatcherLowerMs := c.dnsResponseMatcherLower.Milliseconds()
	dnsRespMatcherCompileMs := c.dnsResponseMatcherCompile.Milliseconds()
	dnsChildSum := dnsUpstreamInitMs + dnsReqProgMs + dnsReqMatcherLowerMs +
		dnsReqMatcherCompileMs + dnsRespProgMs + dnsRespMatcherLowerMs +
		dnsRespMatcherCompileMs
	dnsControllerUnattributedMs := dnsControllerBuildMs - dnsChildSum
	if dnsControllerUnattributedMs < 0 {
		dnsControllerUnattributedMs = 0
	}

	return rulesload.Summary{
		Lifecycle:                       c.lifecycle,
		TotalMs:                         total.Milliseconds(),
		ConfigLoadMs:                    configLoadMs,
		FakeIPAutoExpandMs:              c.fakeipAutoExpand.Milliseconds(),
		DaednsRouterBuildMs:             daednsRouterBuildMs,
		DnsControllerBuildMs:            dnsControllerBuildMs,
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
		DaednsRequestMatcherLowerMs:     daednsReqMatcherLowerMs,
		DaednsRequestMatcherCompileMs:   daednsReqMatcherCompileMs,
		DnsUpstreamInitMs:               dnsUpstreamInitMs,
		DnsRequestProgramNormalizeMs:    dnsReqProgMs,
		DnsRequestMatcherLowerMs:        dnsReqMatcherLowerMs,
		DnsRequestMatcherCompileMs:      dnsReqMatcherCompileMs,
		DnsResponseProgramNormalizeMs:   dnsRespProgMs,
		DnsResponseMatcherLowerMs:       dnsRespMatcherLowerMs,
		DnsResponseMatcherCompileMs:     dnsRespMatcherCompileMs,
		DnsControllerUnattributedMs:     dnsControllerUnattributedMs,
		// daedns_request_matcher_compile_distribution fields.
		DaednsRequestMatcherAcSlots:             c.daednsRequestMatcherAcSlots,
		DaednsRequestMatcherAcPatterns:          c.daednsRequestMatcherAcPatterns,
		DaednsRequestMatcherAcMaxSlotPatterns:   c.daednsRequestMatcherAcMaxSlotPatterns,
		DaednsRequestMatcherAcCpuMs:             c.daednsRequestMatcherAcCpuMs,
		DaednsRequestMatcherAcMaxSlotMs:         c.daednsRequestMatcherAcMaxSlotMs,
		DaednsRequestMatcherTrieSlots:           c.daednsRequestMatcherTrieSlots,
		DaednsRequestMatcherTriePatterns:        c.daednsRequestMatcherTriePatterns,
		DaednsRequestMatcherTrieMaxSlotPatterns: c.daednsRequestMatcherTrieMaxSlotPatterns,
		DaednsRequestMatcherTrieCpuMs:           c.daednsRequestMatcherTrieCpuMs,
		DaednsRequestMatcherTrieMaxSlotMs:       c.daednsRequestMatcherTrieMaxSlotMs,
		DaednsRequestMatcherRegexpSlots:         c.daednsRequestMatcherRegexpSlots,
		DaednsRequestMatcherWallMs:              c.daednsRequestMatcherWallMs,
		// main_routing_matcher_compile_distribution fields.
		MainRoutingMatcherAcSlots:             c.mainRoutingMatcherAcSlots,
		MainRoutingMatcherAcPatterns:          c.mainRoutingMatcherAcPatterns,
		MainRoutingMatcherAcMaxSlotPatterns:   c.mainRoutingMatcherAcMaxSlotPatterns,
		MainRoutingMatcherAcCpuMs:             c.mainRoutingMatcherAcCpuMs,
		MainRoutingMatcherAcMaxSlotMs:         c.mainRoutingMatcherAcMaxSlotMs,
		MainRoutingMatcherTrieSlots:           c.mainRoutingMatcherTrieSlots,
		MainRoutingMatcherTriePatterns:        c.mainRoutingMatcherTriePatterns,
		MainRoutingMatcherTrieMaxSlotPatterns: c.mainRoutingMatcherTrieMaxSlotPatterns,
		MainRoutingMatcherTrieCpuMs:           c.mainRoutingMatcherTrieCpuMs,
		MainRoutingMatcherTrieMaxSlotMs:       c.mainRoutingMatcherTrieMaxSlotMs,
		MainRoutingMatcherRegexpSlots:         c.mainRoutingMatcherRegexpSlots,
		MainRoutingMatcherWallMs:              c.mainRoutingMatcherWallMs,
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
