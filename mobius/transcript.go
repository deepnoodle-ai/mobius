package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/sse"
)

// defaultTranscriptReconnectDelay bounds the pause before reopening a dropped
// session-transcript stream (a connection that ended without a clean
// stream.end rotate). rotate reconnects immediately.
const defaultTranscriptReconnectDelay = time.Second

// IsTerminalTurnStatus reports whether a turn status will not transition
// again. Turn status is an open string in the contract; only these three are
// terminal.
func IsTerminalTurnStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

// TranscriptStreamEvent is a single decoded frame from a session-transcript v2
// stream. EventType mirrors the SSE event: name and the frame's event_type; ID
// is the opaque resume cursor in effect through this frame. The server emits an
// SSE id: line only on state frames that advance the delivered watermark, and
// per the SSE spec the last-event-id persists, so ID carries that watermark and
// is empty only before the connection's first id: line.
//
// Err is set only on the final event WatchSessionTranscript or
// InvokeAgentTranscript emits before giving up on a non-retryable failure — a
// permanent HTTP status such as 401/403/404, or a 5xx other than 503. The
// channel closes immediately after, and Frame is the zero value. Err is nil on
// every decoded frame.
type TranscriptStreamEvent struct {
	EventType string
	ID        string
	Frame     api.SessionTranscriptFrame
	Err       error
}

// SessionTranscriptReducer folds session-transcript v2 frames (or snapshot
// pages) into the authoritative view: message rows keyed by their immutable
// id, turns keyed by id, the opaque resume cursor, and a Ready flag. It is the
// session-scope analogue of Dive's llm.ResponseAccumulator.
//
// The whole merge is Rows[id] = row: state frames carry absolute state, so
// last write wins and nothing is an increment except message.delta text.
// Ignoring deltas entirely still converges.
//
// A reducer is not safe for concurrent use; drive it from a single goroutine
// (e.g. the one receiving a StreamSessionTranscript / WatchSessionTranscript
// channel).
type SessionTranscriptReducer struct {
	// Rows are message rows keyed by their immutable id.
	Rows map[string]*api.SessionTranscriptMessage
	// Turns are turns keyed by id.
	Turns map[string]*api.SessionTranscriptTurn
	// Cursor is the opaque resume cursor; never parse it.
	Cursor string
	// Ready is true once stream.ready has been seen — safe to render.
	Ready bool
}

// NewSessionTranscriptReducer returns an empty reducer.
func NewSessionTranscriptReducer() *SessionTranscriptReducer {
	return &SessionTranscriptReducer{
		Rows:  map[string]*api.SessionTranscriptMessage{},
		Turns: map[string]*api.SessionTranscriptTurn{},
	}
}

// Apply folds one stream frame into the view. sseID is the frame's SSE id:
// line when present; it advances the cursor. Unknown event_types are ignored
// so the protocol can grow without breaking this client.
func (r *SessionTranscriptReducer) Apply(frame api.SessionTranscriptFrame, sseID string) {
	if sseID != "" {
		r.Cursor = sseID
	}
	disc, err := frame.Discriminator()
	if err != nil {
		return
	}
	switch disc {
	case "message.upsert":
		if msg := decodeMessage(frame); msg != nil {
			r.Rows[msg.Id] = msg
		}
	case "message.block":
		mbf, err := frame.AsMessageBlockFrame()
		if err != nil {
			return
		}
		row, ok := r.Rows[mbf.MessageId]
		if !ok || mbf.ContentIndex < 0 {
			return
		}
		// message.block opens (or completes) a block, so it may extend the
		// content slice — unlike patch/delta, which target an existing block.
		for len(row.Content) <= mbf.ContentIndex {
			row.Content = append(row.Content, api.SessionContentBlock{})
		}
		row.Content[mbf.ContentIndex] = mbf.Block
	case "message.block.patch":
		mpf, err := frame.AsMessageBlockPatchFrame()
		if err != nil {
			return
		}
		row, ok := r.Rows[mpf.MessageId]
		if !ok || mpf.ContentIndex < 0 || mpf.ContentIndex >= len(row.Content) {
			return
		}
		progress, progressPresent := frameProgress(frame)
		mutateBlock(&row.Content[mpf.ContentIndex], func(m map[string]interface{}) {
			if mpf.Status != nil {
				m["status"] = *mpf.Status
			}
			if progressPresent {
				if progress == nil {
					delete(m, "progress") // null clears
				} else {
					m["progress"] = progress
				}
			}
			// progress key absent from the patch preserves the existing value
		})
	case "message.delta":
		mdf, err := frame.AsMessageDeltaFrame()
		if err != nil {
			return
		}
		row, ok := r.Rows[mdf.MessageId]
		if !ok || mdf.ContentIndex < 0 || mdf.ContentIndex >= len(row.Content) {
			return
		}
		mutateBlock(&row.Content[mdf.ContentIndex], func(m map[string]interface{}) {
			if mdf.Text != nil && *mdf.Text != "" {
				m["text"] = stringField(m, "text") + *mdf.Text
			}
			if mdf.Thinking != nil && *mdf.Thinking != "" {
				m["thinking"] = stringField(m, "thinking") + *mdf.Thinking
			}
		})
	case "turn.upsert":
		turn := decodeTurn(frame)
		if turn == nil {
			return
		}
		r.Turns[turn.Id] = turn
		if IsTerminalTurnStatus(turn.Status) {
			r.pruneStreamingRows(turn.Id)
		}
	case "stream.ready":
		if srf, err := frame.AsStreamReadyFrame(); err == nil {
			r.Cursor = srf.ResumeCursor // authoritative — adopt unconditionally
			r.Ready = true
		}
	case "stream.end":
		// Control frame; the connection loop acts on it. No state change.
	default:
		// Forward-compatible: ignore unknown frame types.
	}
}

// ApplySnapshot folds a transcript snapshot page (from GetSessionTranscript)
// into the view. Each message folds in as a message.upsert, each turn as a
// turn.upsert. On the final page (HasMore false) the snapshot's streaming rows
// are the complete live set, so any local streaming row absent from it is
// pruned.
func (r *SessionTranscriptReducer) ApplySnapshot(snap *api.SessionTranscriptSnapshot) {
	if snap == nil {
		return
	}
	for i := range snap.Messages {
		msg := snap.Messages[i]
		r.Rows[msg.Id] = &msg
	}
	for i := range snap.Turns {
		turn := snap.Turns[i]
		r.Turns[turn.Id] = &turn
		if IsTerminalTurnStatus(turn.Status) {
			r.pruneStreamingRows(turn.Id)
		}
	}
	if !snap.HasMore {
		live := map[string]struct{}{}
		for i := range snap.Messages {
			if snap.Messages[i].Status == "streaming" {
				live[snap.Messages[i].Id] = struct{}{}
			}
		}
		for id, row := range r.Rows {
			if row.Status == "streaming" {
				if _, ok := live[id]; !ok {
					delete(r.Rows, id)
				}
			}
		}
	}
	r.Cursor = snap.ResumeCursor
}

// Messages returns the rows in render order: final rows by sequence, then
// streaming rows ordered by (turn.created_at, turn.id, turn_index) —
// turn_index alone is unique only within one turn, and turns can run
// concurrently.
func (r *SessionTranscriptReducer) Messages() []*api.SessionTranscriptMessage {
	var final, live []*api.SessionTranscriptMessage
	for _, row := range r.Rows {
		switch row.Status {
		case "final":
			final = append(final, row)
		case "streaming":
			live = append(live, row)
		}
	}
	sort.SliceStable(final, func(i, j int) bool {
		return seqOf(final[i]) < seqOf(final[j])
	})
	sort.SliceStable(live, func(i, j int) bool {
		a, b := live[i], live[j]
		ta, tb := r.turnCreatedAt(a), r.turnCreatedAt(b)
		if !ta.Equal(tb) {
			return ta.Before(tb)
		}
		ida, idb := derefString(a.TurnId), derefString(b.TurnId)
		if ida != idb {
			return ida < idb
		}
		return derefInt(a.TurnIndex) < derefInt(b.TurnIndex)
	})
	return append(final, live...)
}

// MessagesForTurn returns the rows belonging to one turn, in render order.
func (r *SessionTranscriptReducer) MessagesForTurn(turnID string) []*api.SessionTranscriptMessage {
	var out []*api.SessionTranscriptMessage
	for _, row := range r.Messages() {
		if row.TurnId != nil && *row.TurnId == turnID {
			out = append(out, row)
		}
	}
	return out
}

func (r *SessionTranscriptReducer) pruneStreamingRows(turnID string) {
	for id, row := range r.Rows {
		if row.Status == "streaming" && row.TurnId != nil && *row.TurnId == turnID {
			delete(r.Rows, id)
		}
	}
}

func (r *SessionTranscriptReducer) turnCreatedAt(row *api.SessionTranscriptMessage) time.Time {
	if row.TurnId == nil {
		return time.Time{}
	}
	if turn, ok := r.Turns[*row.TurnId]; ok {
		return turn.CreatedAt
	}
	return time.Time{}
}

// GetSessionTranscriptOptions configures GetSessionTranscript.
type GetSessionTranscriptOptions struct {
	// Cursor is an opaque resume cursor from a prior snapshot or stream; omit
	// for a bootstrap tail.
	Cursor string
	// PageToken is the opaque fixed-cut continuation (next_page_token) when
	// draining an incremental cycle.
	PageToken string
	// Limit bounds the messages per page. Zero uses the server default.
	Limit int
}

// StreamSessionTranscriptOptions configures StreamSessionTranscript.
type StreamSessionTranscriptOptions struct {
	// Cursor is an opaque resume cursor; omit to hydrate from the live tail.
	Cursor string
}

// WatchSessionTranscriptOptions configures WatchSessionTranscript.
type WatchSessionTranscriptOptions struct {
	// Cursor is the opaque resume cursor for the first connection.
	Cursor string
	// ReconnectDelay is the pause before reconnecting after a dropped
	// connection (not a clean rotate). Zero uses one second.
	ReconnectDelay time.Duration
}

// GetSessionTranscript fetches a session transcript snapshot (session-stream
// v2). Without a cursor this is a bootstrap tail (latest final page + all live
// rows and turns); with a cursor it drains everything after it toward a fixed
// upper cut — continue with the returned NextPageToken until HasMore is false.
// Fold each page into a SessionTranscriptReducer with ApplySnapshot; polling is
// the same protocol the stream accelerates.
func (c *Client) GetSessionTranscript(ctx context.Context, sessionID string, opts *GetSessionTranscriptOptions) (*api.SessionTranscriptSnapshot, error) {
	params := &api.GetSessionTranscriptParams{}
	if opts != nil {
		if opts.Cursor != "" {
			params.Cursor = &opts.Cursor
		}
		if opts.PageToken != "" {
			params.PageToken = &opts.PageToken
		}
		if opts.Limit > 0 {
			limit := api.LimitParam(opts.Limit)
			params.Limit = &limit
		}
	}
	resp, err := c.ac.GetSessionTranscriptWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: get session transcript: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("get session transcript", resp.Status(), resp.Body)
	}
	return resp.JSON200, nil
}

// StreamSessionTranscript opens one session-transcript SSE connection and
// emits each decoded frame with its SSE id (the resume cursor) on the returned
// channel. The channel is closed when ctx is cancelled or the server closes
// the connection. This is the low-level primitive: apply the frames to a
// SessionTranscriptReducer, or use WatchSessionTranscript for the managed
// connection loop (reconnect on rotate, stop on idle).
func (c *Client) StreamSessionTranscript(ctx context.Context, sessionID string, opts *StreamSessionTranscriptOptions) (<-chan TranscriptStreamEvent, error) {
	params := &api.StreamSessionTranscriptParams{}
	if opts != nil && opts.Cursor != "" {
		params.Cursor = &opts.Cursor
	}
	resp, err := c.ac.StreamSessionTranscript(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), params, acceptEventStream)
	if err != nil {
		return nil, fmt.Errorf("mobius: open session transcript stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("mobius: stream session transcript: unexpected status %d", resp.StatusCode)
	}
	ch := make(chan TranscriptStreamEvent)
	go func() {
		defer close(ch)
		c.pumpTranscript(ctx, resp.Body, "", ch)
	}()
	return ch, nil
}

// WatchSessionTranscript follows a session-transcript stream across the full
// connection lifecycle, emitting every decoded frame on the returned channel.
// It owns the connection loop: it reconnects with the current cursor on a
// stream.end rotate (and after a dropped connection), and closes the channel
// on a stream.end idle or when ctx is cancelled. Apply the frames to a
// SessionTranscriptReducer; reconnect is the same code path as the first
// connect. On idle the caller can poll GetSessionTranscript and reopen when
// ResumeCursor moves.
func (c *Client) WatchSessionTranscript(ctx context.Context, sessionID string, opts *WatchSessionTranscriptOptions) <-chan TranscriptStreamEvent {
	cursor := ""
	delay := defaultTranscriptReconnectDelay
	if opts != nil {
		cursor = opts.Cursor
		if opts.ReconnectDelay > 0 {
			delay = opts.ReconnectDelay
		}
	}
	ch := make(chan TranscriptStreamEvent)
	go c.streamTranscriptLoop(ctx, sessionID, cursor, "", delay, ch)
	return ch
}

// InvokeAgentTranscript behaves like InvokeAgent but returns the started
// turn's transcript stream (session-stream v2) alongside the ack. It starts
// the turn, then opens the transcript stream from the ack's resume cursor. The
// returned channel forwards every frame across reconnects and closes when this
// turn reaches a terminal turn.upsert (or on idle / ctx cancellation). The full
// session stream is consumed internally so the resume cursor stays valid even
// when other turns interleave.
//
// Seed a SessionTranscriptReducer with the ack's UserMessage and Turn, then
// Apply the channel's frames; filter with MessagesForTurn(ack.Turn.Id) to
// render only this turn.
func (c *Client) InvokeAgentTranscript(ctx context.Context, opts InvokeAgentOptions) (*api.TurnAck, <-chan TranscriptStreamEvent, error) {
	ack, err := c.InvokeAgent(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	cursor := ""
	if ack.ResumeCursor != nil {
		cursor = *ack.ResumeCursor
	}
	ch := make(chan TranscriptStreamEvent)
	go c.streamTranscriptLoop(ctx, ack.Session.Id, cursor, ack.Turn.Id, defaultTranscriptReconnectDelay, ch)
	return ack, ch, nil
}

// pumpResult tells streamTranscriptLoop what to do after one connection ends.
type pumpResult int

const (
	pumpStop   pumpResult = iota // idle, stop-turn terminal, or ctx: return
	pumpRotate                   // stream.end rotate: reconnect immediately
	pumpDrop                     // EOF/error: reconnect after the delay
)

// streamTranscriptLoop runs the reconnecting connection loop, forwarding
// frames on ch until it stops, then closes ch. stopTurnID, when set, closes
// the stream after forwarding that turn's terminal turn.upsert.
func (c *Client) streamTranscriptLoop(ctx context.Context, sessionID, cursor, stopTurnID string, delay time.Duration, ch chan<- TranscriptStreamEvent) {
	defer close(ch)
	for {
		if ctx.Err() != nil {
			return
		}
		params := &api.StreamSessionTranscriptParams{}
		if cursor != "" {
			c2 := cursor
			params.Cursor = &c2
		}
		resp, err := c.ac.StreamSessionTranscript(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), params, acceptEventStream)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.config.Logger.Error("open session transcript stream", "error", err)
			if !sleepCtx(ctx, delay) {
				return
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			if ctx.Err() != nil {
				return
			}
			if !isRetryableStreamStatus(resp.StatusCode) {
				// A permanent status (401/403/404, or a 5xx other than 503) is
				// not transient — surface it instead of reconnecting forever.
				emitStreamError(ctx, ch, fmt.Errorf("mobius: session transcript stream: unexpected status %d", resp.StatusCode))
				return
			}
			c.config.Logger.Error("session transcript stream status", "status", resp.StatusCode)
			if !sleepCtx(ctx, delay) {
				return
			}
			continue
		}
		result, next := c.pumpTranscriptLoop(ctx, resp.Body, stopTurnID, cursor, ch)
		cursor = next
		switch result {
		case pumpStop:
			return
		case pumpRotate:
			continue // reconnect immediately
		case pumpDrop:
			if !sleepCtx(ctx, delay) {
				return
			}
		}
	}
}

// pumpTranscript reads one connection to EOF, forwarding frames on ch. It is
// the single-connection reader used by StreamSessionTranscript (no reconnect
// bookkeeping).
func (c *Client) pumpTranscript(ctx context.Context, body io.ReadCloser, stopTurnID string, ch chan<- TranscriptStreamEvent) {
	c.pumpTranscriptLoop(ctx, body, stopTurnID, "", ch)
}

// pumpTranscriptLoop reads one connection to EOF, forwarding frames on ch and
// tracking the resume cursor. It returns the reconnect decision and the latest
// cursor.
func (c *Client) pumpTranscriptLoop(ctx context.Context, body io.ReadCloser, stopTurnID, cursor string, ch chan<- TranscriptStreamEvent) (pumpResult, string) {
	defer func() { _ = body.Close() }()

	reader := sse.NewReader(body)
	reader.Buffer(sseReadBufferSize)

	for {
		if ctx.Err() != nil {
			return pumpStop, cursor
		}
		evt, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return pumpDrop, cursor
		}
		if err != nil {
			if ctx.Err() == nil {
				c.config.Logger.Error("session transcript stream error", "error", err)
			}
			return pumpDrop, cursor
		}
		if evt.Data == "" {
			continue
		}
		var frame api.SessionTranscriptFrame
		if err := frame.UnmarshalJSON([]byte(evt.Data)); err != nil {
			c.config.Logger.Error("parse session transcript frame", "error", err)
			continue
		}
		if evt.ID != "" {
			cursor = evt.ID
		}
		disc, _ := frame.Discriminator()
		if disc == "stream.ready" {
			if srf, err := frame.AsStreamReadyFrame(); err == nil {
				cursor = srf.ResumeCursor
			}
		}
		select {
		case ch <- TranscriptStreamEvent{EventType: evt.Event, ID: evt.ID, Frame: frame}:
		case <-ctx.Done():
			return pumpStop, cursor
		}
		switch disc {
		case "stream.end":
			if sef, err := frame.AsStreamEndFrame(); err == nil {
				if string(sef.Reason) == "idle" {
					return pumpStop, cursor
				}
				return pumpRotate, cursor
			}
		case "turn.upsert":
			if stopTurnID != "" {
				if tuf, err := frame.AsTurnUpsertFrame(); err == nil && tuf.Id == stopTurnID && IsTerminalTurnStatus(tuf.Status) {
					return pumpStop, cursor
				}
			}
		}
	}
}

// decodeMessage converts a message.upsert frame into a SessionTranscriptMessage
// by re-decoding the frame's JSON (the two shapes are field-identical apart
// from event_type, which the message struct ignores).
func decodeMessage(frame api.SessionTranscriptFrame) *api.SessionTranscriptMessage {
	raw, err := frame.MarshalJSON()
	if err != nil {
		return nil
	}
	var msg api.SessionTranscriptMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	return &msg
}

// decodeTurn converts a turn.upsert frame into a SessionTranscriptTurn.
func decodeTurn(frame api.SessionTranscriptFrame) *api.SessionTranscriptTurn {
	raw, err := frame.MarshalJSON()
	if err != nil {
		return nil
	}
	var turn api.SessionTranscriptTurn
	if err := json.Unmarshal(raw, &turn); err != nil {
		return nil
	}
	return &turn
}

// frameProgress reports the frame's progress field: present is false when the
// key is absent (preserve), true with a nil value when it is JSON null (clear).
func frameProgress(frame api.SessionTranscriptFrame) (value interface{}, present bool) {
	raw, err := frame.MarshalJSON()
	if err != nil {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}
	p, ok := fields["progress"]
	if !ok {
		return nil, false
	}
	var v interface{}
	_ = json.Unmarshal(p, &v)
	return v, true
}

// mutateBlock applies fn to a content block as an open JSON map, preserving
// unknown fields — the Go analogue of the reducer's in-place object mutation.
func mutateBlock(block *api.SessionContentBlock, fn func(m map[string]interface{})) {
	raw, err := block.MarshalJSON()
	if err != nil {
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	fn(m)
	updated, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = block.UnmarshalJSON(updated)
}

func stringField(m map[string]interface{}, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func seqOf(msg *api.SessionTranscriptMessage) int {
	if msg.Sequence != nil {
		return *msg.Sequence
	}
	return 0
}

func derefString(s *string) string {
	if s != nil {
		return *s
	}
	return ""
}

func derefInt(i *int) int {
	if i != nil {
		return *i
	}
	return 0
}

// isRetryableStreamStatus reports whether a non-200 stream open should be
// retried by reconnecting. It mirrors the transport retry policy
// (docs/retries.md): only 429 and 503 are transient. Every other status —
// including 401/403/404 and the other 5xx — is surfaced to the caller.
func isRetryableStreamStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// emitStreamError forwards a terminal error event on ch unless ctx is already
// done. The caller (streamTranscriptLoop) closes ch afterward.
func emitStreamError(ctx context.Context, ch chan<- TranscriptStreamEvent, err error) {
	select {
	case ch <- TranscriptStreamEvent{Err: err}:
	case <-ctx.Done():
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
