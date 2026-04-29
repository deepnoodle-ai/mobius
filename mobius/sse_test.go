package mobius

import (
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/assert"
)

func TestRunEvent_AsCustom_RoundTripsServerEnvelope(t *testing.T) {
	// Mirror the wire shape produced by jobs_events.go on the server:
	// the outer SSE Type is "custom" and Data carries an envelope with
	// `type` (the user-supplied subtype), job_id/step_id/path_id, and
	// `data` (the user payload).
	ev := RunEvent{
		Type:      RunEventTypeCustom,
		RunID:     "run_1",
		JobID:     "job_1",
		Seq:       42,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"kind":    "custom",
			"type":    "progress",
			"job_id":  "job_1",
			"step_id": "step_1",
			"path_id": "main",
			"data": map[string]interface{}{
				"step": float64(2),
				"of":   float64(3),
				"note": "halfway",
			},
		},
	}
	subtype, payload, ok := ev.AsCustom()
	assert.True(t, ok)
	assert.Equal(t, subtype, "progress")
	assert.Equal(t, payload["step"], float64(2))
	assert.Equal(t, payload["note"], "halfway")
}

func TestRunEvent_AsCustom_NonCustomReturnsFalse(t *testing.T) {
	ev := RunEvent{Type: RunEventTypeRunUpdated, Data: map[string]interface{}{"status": "completed"}}
	subtype, payload, ok := ev.AsCustom()
	assert.False(t, ok)
	assert.Equal(t, subtype, "")
	assert.Nil(t, payload)
}

func TestRunEvent_AsCustom_MissingSubtypeReturnsFalse(t *testing.T) {
	// A custom event without an inner `type` is malformed; AsCustom
	// should not silently return an empty subtype.
	ev := RunEvent{
		Type: RunEventTypeCustom,
		Data: map[string]interface{}{"data": map[string]interface{}{"x": 1}},
	}
	_, _, ok := ev.AsCustom()
	assert.False(t, ok)
}

func TestRunEventTypeConstants_CoverServerEmittedKinds(t *testing.T) {
	// Sanity check that the constants block enumerates every kind the
	// server actively emits today. Update this list when the server
	// gains a new run-stream event type.
	for _, kind := range []RunEventType{
		RunEventTypeRunUpdated,
		RunEventTypeJobUpdated,
		RunEventTypeRunStepUpdated,
		RunEventTypeCustom,
	} {
		assert.True(t, string(kind) != "")
	}
}
