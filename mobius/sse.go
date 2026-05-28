package mobius

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/sse"
)

// RunEvent is a decoded event from an automation run stream.
type RunEvent = api.AutomationRunEvent

// sseReadBufferSize bounds a single SSE line. Run events can embed step I/O,
// so the bufio default (64KB) is too tight.
const sseReadBufferSize = 8 << 20

// WatchRun opens a Server-Sent Events stream for a single automation run and
// emits decoded RunEvent values on the returned channel. The channel is closed
// when ctx is cancelled or the server closes the connection.
//
// Pass since=0 to start from live updates only; pass a positive sequence cursor
// to replay durable events recorded after that sequence before switching to
// live updates.
func (c *Client) WatchRun(ctx context.Context, runID string, since int64) (<-chan RunEvent, error) {
	resp, err := c.ac.StreamAutomationRunEvents(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(runID), &api.StreamAutomationRunEventsParams{
		SinceSequence: sinceSequenceParam(since),
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

// sinceSequenceParam returns nil for zero so the request omits
// ?since_sequence=0 and the server delivers live-only updates.
func sinceSequenceParam(since int64) *int64 {
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

		var event api.AutomationRunEvent
		if err := evt.JSON(&event); err != nil {
			c.config.Logger.Error("failed to parse SSE event", "error", err)
			continue
		}

		select {
		case ch <- event:
		case <-ctx.Done():
			return
		}
	}
}
