package rulesload

import "github.com/daeuniverse/dae/component/routing/domain_matcher"

// Observer receives notifications of routing build stages during control plane
// construction. Implementations record durations, accumulate summaries, and
// emit structured log events.
type Observer interface {
	// RecordStage is called after a routing build stage completes.
	// stage is one of the Stage* constants in this package.
	// durationMs is wall-clock milliseconds for the stage.
	// rulesIn / rulesOut count rules before/after the stage (0 when unknown).
	//
	// RecordStage only accumulates into the summary. Use EmitStage when the
	// caller also wants a standalone rules_load_stage log line for the stage.
	RecordStage(stage string, durationMs int64, rulesIn, rulesOut int)

	// EmitStage records the stage AND publishes a structured rules_load_stage
	// event. errorClass is the empty string for successful stages, or a short
	// machine-readable token (e.g. "dns_controller_build_error") for failures;
	// a non-empty errorClass marks the event result=error.
	//
	// This is the path used by control plane build sites that don't have
	// direct access to the *logrus.Logger or current Lifecycle; the observer
	// supplies both from the surrounding lifecycle scope.
	EmitStage(stage string, durationMs int64, rulesIn, rulesOut int, errorClass string)

	// EmitDaednsRequestMatcherDistribution records and emits the per-slot
	// breakdown of the heavy AhocorasickSlimtrie.Build inside the daedns
	// router. Must be called after StageDaednsRequestMatcherCompile.
	EmitDaednsRequestMatcherDistribution(stats *domain_matcher.BuildStats)

	// EmitMainRoutingMatcherDistribution records and emits the per-slot
	// breakdown of the heavy AhocorasickSlimtrie.Build inside the main routing
	// matcher. Must be called after StageMainRoutingMatcher.
	EmitMainRoutingMatcherDistribution(stats *domain_matcher.BuildStats)
}

// NoopObserver is an Observer that does nothing. Use as the zero-value default.
type NoopObserver struct{}

func (NoopObserver) RecordStage(string, int64, int, int)       {}
func (NoopObserver) EmitStage(string, int64, int, int, string) {}
func (NoopObserver) EmitDaednsRequestMatcherDistribution(*domain_matcher.BuildStats)  {}
func (NoopObserver) EmitMainRoutingMatcherDistribution(*domain_matcher.BuildStats)    {}
