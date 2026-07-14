package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
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
type TranscriptStreamEvent struct {
	EventType string
	ID        string
	Frame     api.SessionTranscriptFrame
}

// TranscriptConnectionState is observed client transport state, not inferred
// backend state.
type TranscriptConnectionState string

const (
	TranscriptConnectionIdle         TranscriptConnectionState = "idle"
	TranscriptConnectionOpen         TranscriptConnectionState = "open"
	TranscriptConnectionReconnecting TranscriptConnectionState = "reconnecting"
	TranscriptConnectionEnded        TranscriptConnectionState = "ended"
)

// TranscriptUpdate is the last observed frame and the accumulated view after
// it was applied. Pull it from a turn or watcher after Next returns true.
type TranscriptUpdate struct {
	Event          TranscriptStreamEvent
	Cursor         string
	Transcript     *SessionTranscript
	Connection     TranscriptConnectionState
	ReconnectCount int
}

// TranscriptDiagnostics contains observed stream facts only.
type TranscriptDiagnostics struct {
	Status         string
	Cursor         string
	Ready          bool
	ReconnectCount int
	LastFrameType  string
	LastFrameAt    time.Time
	Connection     TranscriptConnectionState
}

// SessionTranscript is the live view of a session: message rows keyed by
// their immutable id, the turns and interactions associated with them, the
// opaque resume cursor, and a Ready flag. It is built by folding session-transcript v2 stream
// frames (or snapshot pages) into place — the session-scope analogue of
// Dive's llm.ResponseAccumulator.
//
// The whole merge is set-by-id: state frames carry absolute state, so last
// write wins and nothing is an increment except message.delta text. Ignoring
// deltas entirely still converges.
//
// The streaming client methods drive one for you: InvokeAgent returns a
// TurnTranscript and WatchSessionTranscript returns a TranscriptWatcher, both
// of which fold frames into an embedded view as you iterate. Construct one
// directly (NewSessionTranscript) only for the escape hatches: polling
// GetSessionTranscript into ApplySnapshot, or feeding StreamSessionTranscript
// frames into Apply.
//
// A SessionTranscript is not safe for concurrent use; drive it from a single
// goroutine.
type SessionTranscript struct {
	rows         map[string]*api.SessionTranscriptMessage
	turns        map[string]*api.SessionTranscriptTurn
	interactions map[string]*api.Interaction
	cursor       string
	ready        bool
}

// NewSessionTranscript returns an empty transcript view.
func NewSessionTranscript() *SessionTranscript {
	return &SessionTranscript{
		rows:         map[string]*api.SessionTranscriptMessage{},
		turns:        map[string]*api.SessionTranscriptTurn{},
		interactions: map[string]*api.Interaction{},
	}
}

// Cursor is the opaque resume cursor in effect through everything folded in
// so far. Never parse it; pass it back via the Cursor option of a snapshot,
// stream, or watch call to resume.
func (t *SessionTranscript) Cursor() string { return t.cursor }

// Ready is true once stream.ready has been seen on the current connection —
// safe to render.
func (t *SessionTranscript) Ready() bool { return t.ready }

// Turn returns the turn with the given id, if present.
func (t *SessionTranscript) Turn(id string) (*api.SessionTranscriptTurn, bool) {
	turn, ok := t.turns[id]
	return turn, ok
}

// Message returns the message row with the given id, if present.
func (t *SessionTranscript) Message(id string) (*api.SessionTranscriptMessage, bool) {
	msg, ok := t.rows[id]
	return msg, ok
}

// Interaction returns the interaction with the given id, if present.
func (t *SessionTranscript) Interaction(id string) (*api.Interaction, bool) {
	interaction, ok := t.interactions[id]
	return interaction, ok
}

// Interactions returns interactions ordered by (created_at, id). Terminal
// interactions observed live remain available; final snapshots only reconcile
// the pending set.
func (t *SessionTranscript) Interactions() []*api.Interaction {
	out := make([]*api.Interaction, 0, len(t.interactions))
	for _, interaction := range t.interactions {
		out = append(out, interaction)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Id < out[j].Id
	})
	return out
}

// PendingInteractions returns the currently pending interactions in stable
// order.
func (t *SessionTranscript) PendingInteractions() []*api.Interaction {
	out := make([]*api.Interaction, 0, len(t.interactions))
	for _, interaction := range t.Interactions() {
		if interaction.Status == api.InteractionStatusPending {
			out = append(out, interaction)
		}
	}
	return out
}

// Turns returns the turns ordered by (created_at, id).
func (t *SessionTranscript) Turns() []*api.SessionTranscriptTurn {
	out := make([]*api.SessionTranscriptTurn, 0, len(t.turns))
	for _, turn := range t.turns {
		out = append(out, turn)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Id < out[j].Id
	})
	return out
}

// Messages returns the rows in render order: final rows by sequence, then
// streaming rows ordered by (turn.created_at, turn.id, turn_index) —
// turn_index alone is unique only within one turn, and turns can run
// concurrently.
func (t *SessionTranscript) Messages() []*api.SessionTranscriptMessage {
	var final, live []*api.SessionTranscriptMessage
	for _, row := range t.rows {
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
		ta, tb := t.turnCreatedAt(a), t.turnCreatedAt(b)
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
func (t *SessionTranscript) MessagesForTurn(turnID string) []*api.SessionTranscriptMessage {
	var out []*api.SessionTranscriptMessage
	for _, row := range t.Messages() {
		if row.TurnId != nil && *row.TurnId == turnID {
			out = append(out, row)
		}
	}
	return out
}

// RenderableMessages returns a policy-light projection for chat and activity
// UIs. It collapses preview/final versions of the same response segment,
// suppresses superseded empty assistant previews, and removes duplicate tool
// blocks by tool-call id. The accumulated lossless rows remain unchanged.
func (t *SessionTranscript) RenderableMessages() []*api.SessionTranscriptMessage {
	ordered := t.Messages()
	chosen := make(map[string]*api.SessionTranscriptMessage, len(ordered))
	order := make([]string, 0, len(ordered))
	for _, row := range ordered {
		copy := cloneTranscriptMessage(row)
		normalizeMessageContent(copy)
		key := renderLogicalKey(copy)
		if _, ok := chosen[key]; !ok {
			order = append(order, key)
		}
		current := chosen[key]
		if current == nil || (copy.Status == "final" && current.Status != "final") ||
			(copy.Status == current.Status && messageCompleteness(copy) >= messageCompleteness(current)) {
			chosen[key] = copy
		}
	}

	// For an active turn, only its newest empty streaming assistant segment is
	// useful. Older empty placeholders are superseded response starts.
	newestEmpty := map[string]string{}
	for _, key := range order {
		row := chosen[key]
		if !isEmptyAssistantPreview(row) || !t.turnIsActive(derefString(row.TurnId)) {
			continue
		}
		turnID := derefString(row.TurnId)
		prevKey := newestEmpty[turnID]
		if prevKey == "" || derefInt(chosen[prevKey].TurnIndex) <= derefInt(row.TurnIndex) {
			newestEmpty[turnID] = key
		}
	}

	rows := make([]*api.SessionTranscriptMessage, 0, len(order))
	for _, key := range order {
		row := chosen[key]
		if isEmptyAssistantPreview(row) && t.turnIsActive(derefString(row.TurnId)) && newestEmpty[derefString(row.TurnId)] != key {
			continue
		}
		rows = append(rows, row)
	}
	deduplicateToolBlocks(rows)
	return rows
}

// RenderableMessagesForTurn is RenderableMessages scoped to one turn.
func (t *SessionTranscript) RenderableMessagesForTurn(turnID string) []*api.SessionTranscriptMessage {
	var out []*api.SessionTranscriptMessage
	for _, row := range t.RenderableMessages() {
		if row.TurnId != nil && *row.TurnId == turnID {
			out = append(out, row)
		}
	}
	return out
}

// Apply folds one stream frame into the view. Unknown event_types are ignored
// so the protocol can grow without breaking this client. This is the escape
// hatch for frames obtained from StreamSessionTranscript or a custom
// transport; the streaming handles call it for you.
func (t *SessionTranscript) Apply(ev TranscriptStreamEvent) {
	if ev.ID != "" {
		t.cursor = ev.ID
	}
	frame := ev.Frame
	disc, err := frame.Discriminator()
	if err != nil {
		return
	}
	switch disc {
	case "message.upsert":
		if msg := decodeMessage(frame); msg != nil {
			t.rows[msg.Id] = msg
		}
	case "message.block":
		mbf, err := frame.AsMessageBlockFrame()
		if err != nil {
			return
		}
		row, ok := t.rows[mbf.MessageId]
		if !ok || mbf.ContentIndex < 0 {
			return
		}
		// message.block opens (or completes) a block, so it may extend the
		// content slice — unlike patch/delta, which target an existing block.
		// Pad with `{}` blocks (not zero values, which marshal as `null`) so
		// later patch/delta frames can mutate them.
		for len(row.Content) <= mbf.ContentIndex {
			row.Content = append(row.Content, emptyContentBlock())
		}
		row.Content[mbf.ContentIndex] = mbf.Block
	case "message.block.patch":
		mpf, err := frame.AsMessageBlockPatchFrame()
		if err != nil {
			return
		}
		row, ok := t.rows[mpf.MessageId]
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
			if mpf.ResolvedAction != nil {
				m["resolved_action"] = map[string]interface{}{
					"name":  mpf.ResolvedAction.Name,
					"input": mpf.ResolvedAction.Input,
				}
			}
			// progress key absent from the patch preserves the existing value
		})
	case "message.delta":
		mdf, err := frame.AsMessageDeltaFrame()
		if err != nil {
			return
		}
		row, ok := t.rows[mdf.MessageId]
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
		t.turns[turn.Id] = turn
		if IsTerminalTurnStatus(turn.Status) {
			t.pruneStreamingRows(turn.Id)
		}
	case "interaction.upsert":
		interaction := decodeInteraction(frame)
		if interaction == nil {
			return
		}
		t.interactions[interaction.Id] = interaction
	case "stream.ready":
		if srf, err := frame.AsStreamReadyFrame(); err == nil {
			t.cursor = srf.ResumeCursor // authoritative — adopt unconditionally
			t.ready = true
		}
	case "stream.end":
		// Control frame; the connection loop acts on it. No state change.
	default:
		// Forward-compatible: ignore unknown frame types.
	}
}

// ApplySnapshot folds a transcript snapshot page (from GetSessionTranscript)
// into the view. Each message, turn, and pending interaction folds in as an
// upsert. On the final page (HasMore false) the snapshot's streaming rows and
// pending interactions are complete sets, so absent local pending state is
// pruned. The snapshot cannot synthesize a terminal status for a missed live
// upsert; terminal interactions already observed live remain in history.
func (t *SessionTranscript) ApplySnapshot(snap *api.SessionTranscriptSnapshot) {
	if snap == nil {
		return
	}
	for i := range snap.Messages {
		msg := snap.Messages[i]
		normalizeMessageContent(&msg)
		t.rows[msg.Id] = &msg
	}
	for i := range snap.Turns {
		turn := snap.Turns[i]
		t.turns[turn.Id] = &turn
		if IsTerminalTurnStatus(turn.Status) {
			t.pruneStreamingRows(turn.Id)
		}
	}
	for i := range snap.Interactions {
		interaction := snap.Interactions[i]
		t.interactions[interaction.Id] = &interaction
	}
	if !snap.HasMore {
		live := map[string]struct{}{}
		for i := range snap.Messages {
			if snap.Messages[i].Status == "streaming" {
				live[snap.Messages[i].Id] = struct{}{}
			}
		}
		for id, row := range t.rows {
			if row.Status == "streaming" {
				if _, ok := live[id]; !ok {
					delete(t.rows, id)
				}
			}
		}
		pending := make(map[string]struct{}, len(snap.Interactions))
		for i := range snap.Interactions {
			pending[snap.Interactions[i].Id] = struct{}{}
		}
		for id, interaction := range t.interactions {
			if interaction.Status == api.InteractionStatusPending {
				if _, ok := pending[id]; !ok {
					delete(t.interactions, id)
				}
			}
		}
	}
	t.cursor = snap.ResumeCursor
}

// Seed folds a turn-start response into the view: the caller's message row,
// the acked turn, and the resume cursor. InvokeAgent calls it for you; it is
// public for callers wiring their own transport around a raw invoke.
func (t *SessionTranscript) Seed(ack *api.TurnAck) {
	if ack == nil {
		return
	}
	if ack.UserMessage != nil {
		msg := *ack.UserMessage
		normalizeMessageContent(&msg)
		t.rows[msg.Id] = &msg
	}
	if turn := turnFromAck(&ack.Turn); turn != nil {
		t.turns[turn.Id] = turn
	}
	if ack.ResumeCursor != "" {
		t.cursor = ack.ResumeCursor
	}
}

func (t *SessionTranscript) pruneStreamingRows(turnID string) {
	for id, row := range t.rows {
		if row.Status == "streaming" && row.TurnId != nil && *row.TurnId == turnID {
			delete(t.rows, id)
		}
	}
}

func (t *SessionTranscript) turnCreatedAt(row *api.SessionTranscriptMessage) time.Time {
	if row.TurnId == nil {
		return time.Time{}
	}
	if turn, ok := t.turns[*row.TurnId]; ok {
		return turn.CreatedAt
	}
	return time.Time{}
}

// TurnTranscript is a started agent turn and its live transcript, returned by
// InvokeAgent. The identity accessors (ID, SessionID, …) are available
// immediately; the transcript stream is lazy — the first Next call opens it,
// so a caller that never iterates pays for nothing beyond the invoke itself.
//
// Iterate with Next/Err, rendering Messages between calls:
//
//	for turn.Next() {
//		render(turn.Messages())
//	}
//	if err := turn.Err(); err != nil { ... }
//
// Next folds frames in the calling goroutine and returns after each state
// change, reconnecting through stream rotations and dropped connections. It
// returns false once this turn reaches a terminal turn.upsert, on stream idle,
// on ctx cancellation, or on a permanent stream error (recorded in Err). Before
// exposing the terminal update it reconciles the incremental durable snapshot,
// so Messages contains the rows committed with completion. The full session
// stream is consumed internally so the resume cursor stays valid when other
// turns interleave; Messages is scoped to this turn, and Transcript exposes the
// whole session view.
type TurnTranscript struct {
	stream        transcriptStream
	turnID        string
	sessionID     string
	afterSequence int64
	deduped       bool
	// invocationCursor is the immutable v2 lower boundary used for initial replay
	// and terminal settlement. stream.view.Cursor continues moving for reconnects.
	invocationCursor string
	// hydrate is set when the acked turn was already terminal (a deduped
	// resume of a completed turn): there is nothing to stream, so the first
	// Next fetches the snapshot (all pages) instead, making Messages complete
	// either way.
	hydrate bool
}

// ID is the turn id.
func (t *TurnTranscript) ID() string { return t.turnID }

// SessionID is the id of the session this turn ran in.
func (t *TurnTranscript) SessionID() string { return t.sessionID }

// Status is the turn's lifecycle status ("queued", "running", "completed",
// …). It is live: each applied turn.upsert updates it.
func (t *TurnTranscript) Status() string {
	if turn, ok := t.stream.view.Turn(t.turnID); ok {
		return turn.Status
	}
	return ""
}

// ErrorType is the Mobius-owned failure category for this turn, when failed.
// It is live and changes as turn.upsert frames are applied.
func (t *TurnTranscript) ErrorType() string {
	if turn, ok := t.stream.view.Turn(t.turnID); ok && turn.ErrorType != nil {
		return *turn.ErrorType
	}
	return ""
}

// ErrorMessage is the human-readable terminal failure message, when present.
func (t *TurnTranscript) ErrorMessage() string {
	if turn, ok := t.stream.view.Turn(t.turnID); ok && turn.ErrorMessage != nil {
		return *turn.ErrorMessage
	}
	return ""
}

// TurnError combines the live turn error type and message. It is nil unless
// the turn is failed; Err separately reports transcript transport failures.
func (t *TurnTranscript) TurnError() error {
	if t.Status() != "failed" {
		return nil
	}
	typ, message := t.ErrorType(), t.ErrorMessage()
	if typ != "" && message != "" {
		return fmt.Errorf("%s: %s", typ, message)
	}
	if message != "" {
		return errors.New(message)
	}
	if typ != "" {
		return errors.New(typ)
	}
	return errors.New("mobius: turn failed")
}

// Deduped reports whether a repeated idempotency key resumed an existing turn
// instead of starting a new one.
func (t *TurnTranscript) Deduped() bool { return t.deduped }

// AfterSequence is the durable v1 stream cursor from the turn-start response;
// pass it as after_sequence to GET …/sessions/{id}/stream (RawClient) to
// follow this turn on the v1 session stream instead.
func (t *TurnTranscript) AfterSequence() int64 { return t.afterSequence }

// Next advances the transcript by one state change, opening the stream on the
// first call. It reports false when the turn is finished (terminal status,
// stream idle, ctx cancelled) or a permanent error occurred — check Err.
func (t *TurnTranscript) Next() bool {
	if t.hydrate {
		t.hydrate = false
		t.stream.done = true
		if err := t.reconcileSnapshot(t.invocationCursor); err != nil {
			t.stream.err = err
			return false
		}
		return true
	}
	if !t.stream.next() {
		return false
	}
	if t.stream.lastFrameType == "turn.upsert" {
		turn, err := t.stream.lastEvent.Frame.AsTurnUpsertFrame()
		if err == nil && turn.Id == t.turnID && IsTerminalTurnStatus(turn.Status) {
			if err := t.reconcileSnapshot(t.invocationCursor); err != nil {
				t.stream.err = fmt.Errorf("mobius: reconcile terminal transcript: %w", err)
				t.stream.stop()
				return false
			}
		}
	}
	return true
}

func (t *TurnTranscript) reconcileSnapshot(cursor string) error {
	opts := &GetSessionTranscriptOptions{Cursor: cursor}
	for {
		snap, err := t.stream.client.GetSessionTranscript(t.stream.ctx, t.sessionID, opts)
		if err != nil {
			return err
		}
		t.stream.view.ApplySnapshot(snap)
		if !snap.HasMore || snap.NextPageToken == nil || *snap.NextPageToken == "" {
			return nil
		}
		opts = &GetSessionTranscriptOptions{PageToken: *snap.NextPageToken}
	}
}

// Err returns the permanent error that ended iteration, if any. A clean
// finish (terminal turn, idle stream, ctx cancellation) returns nil.
func (t *TurnTranscript) Err() error { return t.stream.err }

// Messages returns this turn's rows, in render order.
func (t *TurnTranscript) Messages() []*api.SessionTranscriptMessage {
	return t.stream.view.MessagesForTurn(t.turnID)
}

// RenderableMessages returns this turn's UI-oriented transcript projection.
func (t *TurnTranscript) RenderableMessages() []*api.SessionTranscriptMessage {
	return t.stream.view.RenderableMessagesForTurn(t.turnID)
}

// Update returns the most recently applied protocol update.
func (t *TurnTranscript) Update() TranscriptUpdate { return t.stream.update() }

// Diagnostics returns observed turn/stream facts without guessing backend
// queue or provider state.
func (t *TurnTranscript) Diagnostics() TranscriptDiagnostics {
	diagnostics := t.stream.diagnostics()
	diagnostics.Status = t.Status()
	return diagnostics
}

// Transcript returns the full session view the stream folds into, for callers
// that need rows beyond this turn or the resume cursor.
func (t *TurnTranscript) Transcript() *SessionTranscript { return t.stream.view }

// TranscriptWatcher follows a session's live transcript, returned by
// WatchSessionTranscript. Iterate with Next/Err exactly as with
// TurnTranscript; the view methods (Messages, Turn, Cursor, …) are promoted
// from the embedded SessionTranscript.
//
// Next reconnects with the current cursor on a stream.end rotate and after
// dropped connections. It returns false on a stream.end idle unless Follow
// is enabled, and always stops on ctx cancellation or a permanent stream
// error (recorded in Err).
type TranscriptWatcher struct {
	stream transcriptStream
}

// Next advances the transcript by one state change, opening the stream on the
// first call. It reports false when the stream ends (idle, ctx cancelled) or
// a permanent error occurred — check Err.
func (w *TranscriptWatcher) Next() bool { return w.stream.next() }

// Err returns the permanent error that ended iteration, if any. A clean
// finish (idle stream, ctx cancellation) returns nil.
func (w *TranscriptWatcher) Err() error { return w.stream.err }

// Transcript returns the session view the stream folds into.
func (w *TranscriptWatcher) Transcript() *SessionTranscript { return w.stream.view }

// Messages returns the session's rows in render order.
func (w *TranscriptWatcher) Messages() []*api.SessionTranscriptMessage {
	return w.stream.view.Messages()
}

// RenderableMessages returns the UI-oriented projection of the session rows.
func (w *TranscriptWatcher) RenderableMessages() []*api.SessionTranscriptMessage {
	return w.stream.view.RenderableMessages()
}

// MessagesForTurn returns the rows belonging to one turn, in render order.
func (w *TranscriptWatcher) MessagesForTurn(turnID string) []*api.SessionTranscriptMessage {
	return w.stream.view.MessagesForTurn(turnID)
}

// Turn returns the turn with the given id, if present.
func (w *TranscriptWatcher) Turn(id string) (*api.SessionTranscriptTurn, bool) {
	return w.stream.view.Turn(id)
}

// Message returns the message row with the given id, if present.
func (w *TranscriptWatcher) Message(id string) (*api.SessionTranscriptMessage, bool) {
	return w.stream.view.Message(id)
}

// Turns returns the turns ordered by (created_at, id).
func (w *TranscriptWatcher) Turns() []*api.SessionTranscriptTurn { return w.stream.view.Turns() }

// Cursor is the opaque resume cursor in effect through the last applied
// frame; persist it to resume a later watch from the same position.
func (w *TranscriptWatcher) Cursor() string { return w.stream.view.Cursor() }

// Ready is true once stream.ready has been seen on the current connection.
func (w *TranscriptWatcher) Ready() bool { return w.stream.view.Ready() }

// Update returns the most recently applied protocol update.
func (w *TranscriptWatcher) Update() TranscriptUpdate { return w.stream.update() }

// Diagnostics returns observed session-stream facts.
func (w *TranscriptWatcher) Diagnostics() TranscriptDiagnostics { return w.stream.diagnostics() }

// transcriptStream is the pull-driven engine behind TurnTranscript and
// TranscriptWatcher: one SSE connection at a time, the reconnect policy, and
// the fold into the view. Everything runs in the goroutine calling next —
// there is no producer goroutine, so the view is never written concurrently
// with a read.
type transcriptStream struct {
	client     *Client
	ctx        context.Context
	sessionID  string
	stopTurnID string // when set, stop after this turn's terminal turn.upsert
	delay      time.Duration
	follow     bool
	view       *SessionTranscript

	body   io.ReadCloser
	reader *sse.Reader
	err    error
	done   bool

	lastEvent     TranscriptStreamEvent
	lastFrameType string
	lastFrameAt   time.Time
	connection    TranscriptConnectionState
	reconnects    int
}

// next advances by one applied state frame, connecting/reconnecting as
// needed. Control frames (stream.end) are consumed internally.
func (s *transcriptStream) next() bool {
	if s.done {
		return false
	}
	for {
		if s.ctx.Err() != nil {
			s.stop()
			return false
		}
		if s.reader == nil && !s.connect() {
			return false
		}
		evt, err := s.reader.Read()
		if err != nil {
			// EOF or a read error: drop the connection and retry after the
			// delay; the cursor carried by the view resumes the stream.
			if !errors.Is(err, io.EOF) && s.ctx.Err() == nil {
				s.client.config.Logger.Error("session transcript stream error", "error", err)
			}
			s.disconnect()
			s.connection = TranscriptConnectionReconnecting
			s.reconnects++
			if !sleepCtx(s.ctx, s.delay) {
				s.stop()
				return false
			}
			continue
		}
		if evt.Data == "" {
			continue
		}
		var frame api.SessionTranscriptFrame
		if err := frame.UnmarshalJSON([]byte(evt.Data)); err != nil {
			s.client.config.Logger.Error("parse session transcript frame", "error", err)
			continue
		}
		disc, _ := frame.Discriminator()
		if disc == "stream.end" {
			if sef, err := frame.AsStreamEndFrame(); err == nil && string(sef.Reason) == "idle" {
				if s.follow {
					s.disconnect()
					s.connection = TranscriptConnectionReconnecting
					s.reconnects++
					if !sleepCtx(s.ctx, s.delay) {
						s.stop()
						return false
					}
					continue
				}
				s.connection = TranscriptConnectionEnded
				s.stop()
				return false
			}
			s.disconnect() // rotate: reconnect immediately with the current cursor
			s.connection = TranscriptConnectionReconnecting
			s.reconnects++
			continue
		}
		update := TranscriptStreamEvent{EventType: evt.Event, ID: evt.ID, Frame: frame}
		s.view.Apply(update)
		s.lastEvent = update
		s.lastFrameType = disc
		s.lastFrameAt = time.Now()
		if s.stopTurnID != "" && disc == "turn.upsert" {
			if tuf, err := frame.AsTurnUpsertFrame(); err == nil && tuf.Id == s.stopTurnID && IsTerminalTurnStatus(tuf.Status) {
				s.stop() // yield the terminal state; the next call reports false
			}
		}
		return true
	}
}

// connect opens the SSE connection from the view's cursor, retrying transient
// failures. It reports false when iteration should stop (ctx cancelled or a
// permanent status, recorded in err).
func (s *transcriptStream) connect() bool {
	for {
		if s.ctx.Err() != nil {
			s.done = true
			return false
		}
		params := &api.StreamSessionTranscriptParams{}
		if cursor := s.view.Cursor(); cursor != "" {
			params.Cursor = &cursor
		}
		resp, err := s.client.ac.StreamSessionTranscript(s.ctx, api.ProjectHandleParam(s.client.projectHandle), api.SessionIdParam(s.sessionID), params, acceptEventStream)
		if err != nil {
			if s.ctx.Err() != nil {
				s.done = true
				return false
			}
			s.client.config.Logger.Error("open session transcript stream", "error", err)
			if !sleepCtx(s.ctx, s.delay) {
				s.done = true
				return false
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if s.ctx.Err() != nil {
				s.done = true
				return false
			}
			if !isRetryableStreamStatus(resp.StatusCode) {
				// A permanent status (401/403/404, or a 5xx other than 503) is
				// not transient — surface it instead of reconnecting forever.
				s.err = unexpectedAPIStatus("stream session transcript", resp.StatusCode, resp.Status, resp.Header, body)
				s.done = true
				return false
			}
			s.client.config.Logger.Error("session transcript stream status", "status", resp.StatusCode)
			if !sleepCtx(s.ctx, s.delay) {
				s.done = true
				return false
			}
			continue
		}
		s.body = resp.Body
		s.reader = sse.NewReader(resp.Body)
		s.reader.Buffer(sseReadBufferSize)
		s.view.ready = false // ready is per-connection; stream.ready re-arms it
		s.connection = TranscriptConnectionOpen
		s.client.config.Logger.Debug("mobius: session transcript stream opened",
			"session_id", s.sessionID,
			"reconnect_count", s.reconnects,
		)
		return true
	}
}

func (s *transcriptStream) disconnect() {
	if s.body != nil {
		_ = s.body.Close()
	}
	s.body = nil
	s.reader = nil
}

func (s *transcriptStream) stop() {
	s.disconnect()
	s.done = true
	if s.connection != TranscriptConnectionEnded {
		s.connection = TranscriptConnectionEnded
	}
}

func (s *transcriptStream) update() TranscriptUpdate {
	return TranscriptUpdate{
		Event:          s.lastEvent,
		Cursor:         s.view.Cursor(),
		Transcript:     s.view,
		Connection:     s.connection,
		ReconnectCount: s.reconnects,
	}
}

func (s *transcriptStream) diagnostics() TranscriptDiagnostics {
	return TranscriptDiagnostics{
		Cursor:         s.view.Cursor(),
		Ready:          s.view.Ready(),
		ReconnectCount: s.reconnects,
		LastFrameType:  s.lastFrameType,
		LastFrameAt:    s.lastFrameAt,
		Connection:     s.connection,
	}
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
	// Cursor is the opaque resume cursor for the first connection. Ignored if
	// Transcript already carries one.
	Cursor string
	// Transcript is an existing view to continue folding into (e.g. one
	// bootstrapped from GetSessionTranscript pages). Omit to start fresh.
	Transcript *SessionTranscript
	// ReconnectDelay is the pause before reconnecting after a dropped
	// connection (not a clean rotate). Zero uses one second.
	ReconnectDelay time.Duration
	// Follow reconnects after an idle close so turns started later are
	// observed. The default false preserves request/response behavior.
	Follow bool
}

// GetSessionTranscript fetches a session transcript snapshot (session-stream
// v2). Without a cursor this is a bootstrap tail (latest final page + all live
// rows and turns); with a cursor it drains everything after it toward a fixed
// upper cut — continue with the returned NextPageToken until HasMore is false.
// Fold each page into a SessionTranscript with ApplySnapshot; polling is the
// same protocol the stream accelerates.
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
		return nil, unexpectedSessionStatus("get session transcript", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	if resp.JSON200.Interactions == nil {
		resp.JSON200.Interactions = []api.Interaction{}
	}
	return resp.JSON200, nil
}

// StreamSessionTranscript opens one session-transcript SSE connection and
// emits each decoded frame with its SSE id (the resume cursor) on the returned
// channel. The channel is closed when ctx is cancelled, the server closes the
// connection, or a stream.end frame arrives (after forwarding it). This is the
// low-level primitive: apply the frames to a SessionTranscript, or use
// WatchSessionTranscript for the managed connection loop (reconnect on rotate,
// stop on idle).
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
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, unexpectedAPIStatus("stream session transcript", resp.StatusCode, resp.Status, resp.Header, body)
	}
	ch := make(chan TranscriptStreamEvent)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		reader := sse.NewReader(resp.Body)
		reader.Buffer(sseReadBufferSize)
		for {
			if ctx.Err() != nil {
				return
			}
			evt, err := reader.Read()
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					c.config.Logger.Error("session transcript stream error", "error", err)
				}
				return
			}
			if evt.Data == "" {
				continue
			}
			var frame api.SessionTranscriptFrame
			if err := frame.UnmarshalJSON([]byte(evt.Data)); err != nil {
				c.config.Logger.Error("parse session transcript frame", "error", err)
				continue
			}
			select {
			case ch <- TranscriptStreamEvent{EventType: evt.Event, ID: evt.ID, Frame: frame}:
			case <-ctx.Done():
				return
			}
			if disc, _ := frame.Discriminator(); disc == "stream.end" {
				return
			}
		}
	}()
	return ch, nil
}

// WatchSessionTranscript follows a session's live transcript across the full
// connection lifecycle. The returned watcher owns the connection loop and the
// view; iterate with Next/Err and read Messages between calls. The stream is
// lazy — the first Next opens it.
func (c *Client) WatchSessionTranscript(ctx context.Context, sessionID string, opts *WatchSessionTranscriptOptions) *TranscriptWatcher {
	view := (*SessionTranscript)(nil)
	cursor := ""
	delay := defaultTranscriptReconnectDelay
	follow := false
	if opts != nil {
		view = opts.Transcript
		cursor = opts.Cursor
		if opts.ReconnectDelay > 0 {
			delay = opts.ReconnectDelay
		}
		follow = opts.Follow
	}
	if view == nil {
		view = NewSessionTranscript()
	}
	if cursor != "" && view.cursor == "" {
		view.cursor = cursor
	}
	return &TranscriptWatcher{stream: transcriptStream{
		client:     c,
		ctx:        ctx,
		sessionID:  sessionID,
		delay:      delay,
		follow:     follow,
		view:       view,
		connection: TranscriptConnectionIdle,
	}}
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
	normalizeMessageContent(&msg)
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

// decodeInteraction converts an interaction.upsert frame into the shared
// Interaction model. The frame adds only event_type and a frame-local status
// enum, so a JSON round-trip is the tolerant conversion.
func decodeInteraction(frame api.SessionTranscriptFrame) *api.Interaction {
	raw, err := frame.MarshalJSON()
	if err != nil {
		return nil
	}
	var interaction api.Interaction
	if err := json.Unmarshal(raw, &interaction); err != nil {
		return nil
	}
	return &interaction
}

// turnFromAck converts the ack's AgentTurn into the transcript's turn shape.
// The two structs are field-identical apart from the status type, so a JSON
// round-trip is the tolerant conversion.
func turnFromAck(turn *api.AgentTurn) *api.SessionTranscriptTurn {
	raw, err := json.Marshal(turn)
	if err != nil {
		return nil
	}
	var out api.SessionTranscriptTurn
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return &out
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
// unknown fields — the Go analogue of the reducer-style in-place mutation.
func emptyContentBlock() api.SessionContentBlock {
	var b api.SessionContentBlock
	_ = b.UnmarshalJSON([]byte("{}"))
	return b
}

func mutateBlock(block *api.SessionContentBlock, fn func(m map[string]interface{})) {
	raw, err := block.MarshalJSON()
	if err != nil {
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	if m == nil {
		m = map[string]interface{}{} // a `null` block (zero value) still mutates safely
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

// NormalizedToolUse exposes both the provider-safe wire identity and the
// canonical catalog action, when the backend resolved one.
type NormalizedToolUse struct {
	WireName       string
	WireInput      map[string]interface{}
	ResolvedAction *api.SessionResolvedAction
	ToolkitName    string
	Command        string
	Raw            api.SessionToolUseBlock
}

// NormalizeToolUse prefers the persisted canonical resolved_action and falls
// back to the legacy meta-router {command,args} shape. A concatenated
// wireName.command value is only a compatibility heuristic.
func NormalizeToolUse(block api.SessionToolUseBlock) NormalizedToolUse {
	out := NormalizedToolUse{
		WireName:       block.Name,
		WireInput:      block.Input,
		ResolvedAction: block.ResolvedAction,
		Raw:            block,
	}
	canonical := ""
	if block.ResolvedAction != nil {
		canonical = block.ResolvedAction.Name
	} else if command, ok := block.Input["command"].(string); ok && command != "" {
		out.Command = command
		out.ToolkitName = block.Name
		if args, ok := block.Input["args"].(map[string]interface{}); ok {
			out.ResolvedAction = &api.SessionResolvedAction{
				Name:  block.Name + "." + command,
				Input: args,
			}
		}
	}
	if canonical != "" && block.ResolvedAction != nil {
		if dot := strings.Index(canonical, "."); dot > 0 {
			out.ToolkitName = canonical[:dot]
			out.Command = canonical[dot+1:]
		}
	}
	return out
}

// TextOf concatenates text blocks in a transcript message.
func TextOf(message *api.SessionTranscriptMessage) string {
	if message == nil {
		return ""
	}
	var b strings.Builder
	for _, block := range message.Content {
		if text, err := block.AsSessionTextBlock(); err == nil {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// ToolResultText returns the text representation of a tool_result payload,
// handling both its string and block-array wire forms.
func ToolResultText(block api.SessionToolResultBlock) string {
	if block.Content == nil {
		return ""
	}
	if text, err := block.Content.AsSessionToolResultBlockContent0(); err == nil {
		return text
	}
	blocks, err := block.Content.AsSessionToolResultBlockContent1()
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, child := range blocks {
		if text, err := child.AsSessionTextBlock(); err == nil {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func normalizeMessageContent(message *api.SessionTranscriptMessage) {
	if message != nil && message.Content == nil {
		message.Content = []api.SessionContentBlock{}
	}
}

func cloneTranscriptMessage(message *api.SessionTranscriptMessage) *api.SessionTranscriptMessage {
	if message == nil {
		return &api.SessionTranscriptMessage{Content: []api.SessionContentBlock{}}
	}
	raw, err := json.Marshal(message)
	if err != nil {
		copy := *message
		normalizeMessageContent(&copy)
		return &copy
	}
	var copy api.SessionTranscriptMessage
	if err := json.Unmarshal(raw, &copy); err != nil {
		copy = *message
	}
	normalizeMessageContent(&copy)
	return &copy
}

func renderLogicalKey(message *api.SessionTranscriptMessage) string {
	if message.TurnId != nil && *message.TurnId != "" {
		if message.TurnIndex != nil {
			return fmt.Sprintf("logical:%s:%s:%d", *message.TurnId, message.Role, *message.TurnIndex)
		}
		if message.Metadata != nil {
			if index, ok := (*message.Metadata)["response_message_index"]; ok {
				return fmt.Sprintf("logical:%s:%s:%v", *message.TurnId, message.Role, index)
			}
		}
	}
	return "id:" + message.Id
}

func messageCompleteness(message *api.SessionTranscriptMessage) int {
	raw, _ := json.Marshal(message.Content)
	return len(raw)
}

func isEmptyAssistantPreview(message *api.SessionTranscriptMessage) bool {
	return message != nil && message.Status == "streaming" && string(message.Role) == "assistant" && len(message.Content) == 0
}

func (t *SessionTranscript) turnIsActive(turnID string) bool {
	turn, ok := t.turns[turnID]
	return ok && !IsTerminalTurnStatus(turn.Status)
}

func deduplicateToolBlocks(messages []*api.SessionTranscriptMessage) {
	type candidate struct {
		message int
		block   int
		size    int
	}
	best := map[string]candidate{}
	for mi, message := range messages {
		for bi, block := range message.Content {
			key := toolBlockKey(block)
			if key == "" {
				continue
			}
			raw, _ := block.MarshalJSON()
			if current, ok := best[key]; !ok || len(raw) >= current.size {
				best[key] = candidate{message: mi, block: bi, size: len(raw)}
			}
		}
	}
	for mi, message := range messages {
		content := make([]api.SessionContentBlock, 0, len(message.Content))
		for bi, block := range message.Content {
			key := toolBlockKey(block)
			if key == "" || (best[key].message == mi && best[key].block == bi) {
				content = append(content, block)
			}
		}
		message.Content = content
	}
}

func toolBlockKey(block api.SessionContentBlock) string {
	if use, err := block.AsSessionToolUseBlock(); err == nil && use.Id != "" {
		return "use:" + use.Id
	}
	if result, err := block.AsSessionToolResultBlock(); err == nil && result.ToolUseId != "" {
		return "result:" + result.ToolUseId
	}
	return ""
}

// isRetryableStreamStatus reports whether a non-200 stream open should be
// retried by reconnecting. It mirrors the transport retry policy
// (docs/retries.md): only 429 and 503 are transient. Every other status —
// including 401/403/404 and the other 5xx — is surfaced to the caller.
func isRetryableStreamStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
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
