package cmd

import (
	"sync"
	"time"

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
	return rulesload.Summary{
		Lifecycle:             c.lifecycle,
		TotalMs:               total.Milliseconds(),
		ConfigLoadMs:          c.configLoadDur.Milliseconds(),
		FakeIPAutoExpandMs:    c.fakeipAutoExpand.Milliseconds(),
		DaednsRouterBuildMs:   c.daednsRouterBuild.Milliseconds(),
		DnsControllerBuildMs:  c.dnsControllerBuild.Milliseconds(),
		MainRoutingOptimizeMs: c.mainRoutingOptimize.Milliseconds(),
		MainRoutingMatcherMs:  c.mainRoutingMatcher.Milliseconds(),
		ControlPlaneBuildMs:   c.controlPlaneBuild.Milliseconds(),
		ReloadHandoffMs:       c.reloadHandoff.Milliseconds(),
		ReloadRetireMs:        c.reloadRetire.Milliseconds(),
		MainRoutingRules:      c.mainRoutingRules,
		DnsRequestRules:       c.dnsRequestRules,
		DnsResponseRules:      c.dnsResponseRules,
		FakeIPAutoDerived:     c.fakeipAutoDerived,
		RulesTotal:            c.mainRoutingRules + c.dnsRequestRules + c.dnsResponseRules + c.fakeipAutoDerived,
		ErrorClass:            c.errorClass,
	}
}
