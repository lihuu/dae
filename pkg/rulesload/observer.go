package rulesload

// Observer receives notifications of routing build stages during control plane
// construction. Implementations record durations, accumulate summaries, and
// emit structured log events.
type Observer interface {
	// RecordStage is called after a routing build stage completes.
	// stage is one of the Stage* constants in this package.
	// durationMs is wall-clock milliseconds for the stage.
	// rulesIn / rulesOut count rules before/after the stage (0 when unknown).
	RecordStage(stage string, durationMs int64, rulesIn, rulesOut int)
}

// NoopObserver is an Observer that does nothing. Use as the zero-value default.
type NoopObserver struct{}

func (NoopObserver) RecordStage(string, int64, int, int) {}