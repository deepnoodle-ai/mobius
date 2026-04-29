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

func TestRunEvent_AsCustom_MissingPayloadReturnsFalse(t *testing.T) {
	// Subtype present but inner `data` missing entirely — wire format
	// always carries the user payload, so missing data is malformed.
	ev := RunEvent{
		Type: RunEventTypeCustom,
		Data: map[string]interface{}{"type": "progress"},
	}
	_, payload, ok := ev.AsCustom()
	assert.False(t, ok)
	assert.Nil(t, payload)
}

func TestRunEvent_AsCustom_NonObjectPayloadReturnsFalse(t *testing.T) {
	// `data` present but not a JSON object (e.g. user erroneously
	// emitted a string) — AsCustom contract is to return ok=false so
	// callers don't dereference a nil map.
	ev := RunEvent{
		Type: RunEventTypeCustom,
		Data: map[string]interface{}{"type": "progress", "data": "not-a-map"},
	}
	_, payload, ok := ev.AsCustom()
	assert.False(t, ok)
	assert.Nil(t, payload)
}

func TestRunEventTypeConstants_CoverServerEmittedKinds(t *testing.T) {
	// Pin the wire-string values so a rename of a constant in Go does
	// not silently change the SSE type clients dispatch on. Update both
	// the SDK constant and this test (and the server emitter) when an
	// event kind is renamed.
	assert.Equal(t, string(RunEventTypeRunUpdated), "run_updated")
	assert.Equal(t, string(RunEventTypeJobUpdated), "job_updated")
	assert.Equal(t, string(RunEventTypeRunStepUpdated), "run_step_updated")
	assert.Equal(t, string(RunEventTypeCustom), "custom")
}
