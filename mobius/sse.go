package mobius

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/sse"
)

// RunEventType identifies the kind of Server-Sent Event for a run.
type RunEventType string

const (
	RunEventTypeRunUpdated     RunEventType = "run_updated"
	RunEventTypeJobUpdated     RunEventType = "job_updated"
	RunEventTypeActionAppended RunEventType = "action_appended"
)

// RunEvent is a decoded Server-Sent Event from the run event stream.
type RunEvent struct {
	// Type is the event type (run_updated, job_updated, action_appended).
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

// sseReadBufferSize bounds a single SSE line. Run events embed step I/O, so
// the bufio default (64KB) is too tight.
const sseReadBufferSize = 8 << 20

// WatchRun opens a Server-Sent Events stream for a single run and emits
// decoded RunEvent values on the returned channel. The channel is closed
// when ctx is cancelled or the server closes the connection.
//
// Pass since=0 to start from live updates only; pass a positive seq cursor
// to replay durable events recorded since that point before switching to
// live updates.
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
	resp, err := c.ac.StreamRunEvents(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID), &api.StreamRunEventsParams{
		Since: sinceParam(since),
	})
	if err != nil {
		return nil, fmt.Errorf("mobius: open run stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("mobius: stream run events: unexpected status %d", resp.StatusCode)
	}

	ch := make(chan RunEvent)
	go c.readSSEStream(ctx, resp.Body, ch)
	return ch, nil
}

// WatchProjectRuns opens a project-wide Server-Sent Events stream and emits
// decoded RunEvent values on the returned channel. The channel is closed
// when ctx is cancelled or the server closes the connection.
//
// Pass since=0 to start from live updates only; pass a positive seq cursor
// to replay durable events recorded since that point before switching to
// live updates.
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
	resp, err := c.ac.StreamProjectRunEvents(ctx, api.ProjectHandleParam(c.projectHandle), &api.StreamProjectRunEventsParams{
		Since: sinceParam(since),
	})
	if err != nil {
		return nil, fmt.Errorf("mobius: open project stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("mobius: stream project events: unexpected status %d", resp.StatusCode)
	}

	ch := make(chan RunEvent)
	go c.readSSEStream(ctx, resp.Body, ch)
	return ch, nil
}

// sinceParam returns nil for zero so the request omits ?since=0 and the
// server delivers live-only updates. Positive values replay from that cursor.
func sinceParam(since int64) *int64 {
	if since <= 0 {
		return nil
	}
	return &since
}

// readSSEStream decodes SSE frames from body using wonton/sse and forwards
// them on ch. The body is closed when the stream ends or ctx is cancelled.
func (c *Client) readSSEStream(ctx context.Context, body io.ReadCloser, ch chan<- RunEvent) {
	defer close(ch)
	defer func() { _ = body.Close() }()

	reader := sse.NewReader(body)
	reader.Buffer(sseReadBufferSize)

	for {
		if ctx.Err() != nil {
			return
		}

		evt, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				c.config.Logger.Error("SSE stream error", "error", err)
			}
			return
		}

		var env sseEnvelope
		if err := evt.JSON(&env); err != nil {
			c.config.Logger.Error("failed to parse SSE event", "error", err)
			continue
		}

		select {
		case ch <- RunEvent{
			Type:      RunEventType(env.Type),
			RunID:     env.RunID,
			Seq:       env.Seq,
			Timestamp: env.Timestamp,
			Data:      env.Data,
		}:
		case <-ctx.Done():
			return
		}
	}
}
