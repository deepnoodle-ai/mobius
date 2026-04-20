package mobius

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// RunEventType identifies the kind of Server-Sent Event for a run.
type RunEventType string

const (
	RunEventTypeRunUpdated     RunEventType = "run_updated"
	RunEventTypeStepProgress   RunEventType = "step_progress"
	RunEventTypeActionAppended RunEventType = "action_appended"
)

// RunEvent is a decoded Server-Sent Event from the run event stream.
type RunEvent struct {
	// Type is the event type (run_updated, step_progress, action_appended).
	Type RunEventType
	// RunID is the workflow run ID.
	RunID string
	// Seq is the monotonic sequence number for this event in the run's timeline.
	Seq int64
	// Timestamp is when the event was emitted by the server.
	Timestamp time.Time
	// Data is the raw event payload, structured as map[string]interface{}.
	Data map[string]interface{}
}

// sseEnvelope matches the wire format of SSE events from /runs/events and /runs/{id}/events.
type sseEnvelope struct {
	Type      string                 `json:"type"`
	RunID     string                 `json:"run_id"`
	Seq       int64                  `json:"seq"`
	Timestamp time.Time              `json:"timestamp"`
	Data      map[string]interface{} `json:"data"`
}

// WatchRun opens a Server-Sent Events stream for a single run and emits
// decoded RunEvent values on the returned channel. The channel is closed
// when ctx is cancelled or the server closes the connection.
//
// Example:
//
//	events, err := client.WatchRun(ctx, runID, 0)
//	if err != nil {
//		return err
//	}
//	for ev := range events {
//		fmt.Printf("Event: %v (seq %d)\n", ev.Type, ev.Seq)
//	}
func (c *Client) WatchRun(ctx context.Context, runID string, since int64) (<-chan RunEvent, error) {
	resp, err := c.ac.StreamRunEventsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID), &api.StreamRunEventsParams{
		Since: &since,
	})
	if err != nil {
		return nil, fmt.Errorf("mobius: open run stream: %w", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("mobius: stream run events: unexpected status %d", resp.StatusCode())
	}

	ch := make(chan RunEvent)
	go c.readSSEStream(ctx, resp.HTTPResponse.Body, ch)
	return ch, nil
}

// WatchProjectRuns opens a project-wide Server-Sent Events stream and emits
// decoded RunEvent values on the returned channel. The channel is closed
// when ctx is cancelled or the server closes the connection.
//
// The since parameter allows resuming from a known sequence number.
// Pass 0 to start from the beginning of the available history.
//
// Example:
//
//	events, err := client.WatchProjectRuns(ctx, 0)
//	if err != nil {
//		return err
//	}
//	for ev := range events {
//		fmt.Printf("Run %s event: %v (seq %d)\n", ev.RunID, ev.Type, ev.Seq)
//	}
func (c *Client) WatchProjectRuns(ctx context.Context, since int64) (<-chan RunEvent, error) {
	resp, err := c.ac.StreamProjectRunEventsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), &api.StreamProjectRunEventsParams{
		Since: &since,
	})
	if err != nil {
		return nil, fmt.Errorf("mobius: open project stream: %w", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("mobius: stream project events: unexpected status %d", resp.StatusCode())
	}

	ch := make(chan RunEvent)
	go c.readSSEStream(ctx, resp.HTTPResponse.Body, ch)
	return ch, nil
}

// readSSEStream reads from an SSE stream and emits decoded events on the channel.
func (c *Client) readSSEStream(ctx context.Context, body interface{}, ch chan<- RunEvent) {
	defer close(ch)

	// Type assertion for body would normally be checked here.
	// In practice, resp.HTTPResponse.Body is an io.ReadCloser.
	rc, ok := body.(interface {
		Read([]byte) (int, error)
		Close() error
	})
	if !ok {
		// If body doesn't match expected interface, close and return
		return
	}
	defer func() { _ = rc.Close() }()

	scanner := bufio.NewScanner(rc)
	var currentEvent *sseEnvelope

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			// Empty line signals end of event; emit if we have data
			if currentEvent != nil {
				select {
				case ch <- RunEvent{
					Type:      RunEventType(currentEvent.Type),
					RunID:     currentEvent.RunID,
					Seq:       currentEvent.Seq,
					Timestamp: currentEvent.Timestamp,
					Data:      currentEvent.Data,
				}:
				case <-ctx.Done():
					return
				}
				currentEvent = nil
			}
			continue
		}

		// Parse "data: <json>" line
		if len(line) > 6 && string(line[:6]) == "data: " {
			var env sseEnvelope
			if err := json.Unmarshal(line[6:], &env); err != nil {
				// Log error but continue
				c.config.Logger.Error("failed to parse SSE event", "error", err)
				continue
			}
			currentEvent = &env
		}
	}

	// Check for scanner errors (not EOF)
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		c.config.Logger.Error("SSE stream error", "error", err)
	}
}
