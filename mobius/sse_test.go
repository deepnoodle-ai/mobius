package mobius

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
)

func TestSinceSequenceParam(t *testing.T) {
	assert.Nil(t, sinceSequenceParam(0))
	got := sinceSequenceParam(42)
	assert.NotNil(t, got)
	assert.Equal(t, *got, int64(42))
}

func TestReadSSEStream_DecodesAutomationRunEvent(t *testing.T) {
	c, _ := NewClient()
	body := io.NopCloser(strings.NewReader(`event: run.started
id: 7
data: {"id":"evt_1","org_id":"org_1","project_id":"proj_1","run_id":"run_1","event_type":"run.started","sequence":7,"payload":{"loop_id":"loop_1"},"created_at":"2026-05-27T00:00:00Z"}

`))
	ch := make(chan RunEvent)
	go c.readSSEStream(context.Background(), body, ch)

	event, ok := <-ch
	assert.True(t, ok)
	assert.Equal(t, event.Id, "evt_1")
	assert.Equal(t, event.RunId, "run_1")
	assert.Equal(t, event.Sequence, int64(7))
	assert.Equal(t, event.EventType, "run.started")
	assert.NotNil(t, event.Payload)
	payload, err := event.Payload.AsRunStartedPayload()
	assert.Nil(t, err)
	assert.NotNil(t, payload.LoopId)
	assert.Equal(t, *payload.LoopId, "loop_1")
	_, ok = <-ch
	assert.False(t, ok)
}
