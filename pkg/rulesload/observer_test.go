package rulesload

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNoopObserver_DoesNotPanic(t *testing.T) {
	o := NoopObserver{}
	// Must not panic on any input.
	o.RecordStage("", 0, 0, 0)
	o.RecordStage(StageReadConfig, -1, -1, -1)
	o.RecordStage(StageMainRoutingMatcher, 1000, 50, 200)
}

// summaryRecorder implements Observer by accumulating calls into a slice.
type summaryRecorder struct {
	calls []stageCall
}

type stageCall struct {
	stage       string
	durationMs  int64
	rulesIn     int
	rulesOut    int
}

func (r *summaryRecorder) RecordStage(stage string, durationMs int64, rulesIn, rulesOut int) {
	r.calls = append(r.calls, stageCall{stage, durationMs, rulesIn, rulesOut})
}

func TestSummaryRecorder_RecordsAllCalls(t *testing.T) {
	r := &summaryRecorder{}
	r.RecordStage("a", 100, 5, 10)
	r.RecordStage("b", 200, 3, 8)

	assert.Len(t, r.calls, 2)
	assert.Equal(t, "a", r.calls[0].stage)
	assert.Equal(t, int64(100), r.calls[0].durationMs)
	assert.Equal(t, 5, r.calls[0].rulesIn)
	assert.Equal(t, 10, r.calls[0].rulesOut)

	assert.Equal(t, "b", r.calls[1].stage)
	assert.Equal(t, int64(200), r.calls[1].durationMs)
	assert.Equal(t, 3, r.calls[1].rulesIn)
	assert.Equal(t, 8, r.calls[1].rulesOut)
}