package mobius

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// CreateSessionRequest resolves or creates a durable session for an agent.
// Set SessionKey to a stable conversation key (a Slack thread, a support
// ticket, a user id) so repeat calls resolve the same session.
type CreateSessionRequest = api.CreateSessionRequest

// SessionInputDelivery says where SendSessionInput puts the input.
type SessionInputDelivery string

const (
	// DeliverNewTurn appends the input as a user message and starts an agent
	// turn to respond. It is the default. A send while another turn is
	// queued, running, or waiting fails with 409 session_turn_active.
	DeliverNewTurn SessionInputDelivery = "new_turn"
	// DeliverCurrentTurn steers the newest turn that can still absorb the
	// input; the agent receives it at the turn's next safe boundary. When no
	// turn can take it, the server starts a new turn instead. Text blocks
	// only.
	DeliverCurrentTurn SessionInputDelivery = "current_turn"
	// DeliveredQueued is reported (never requested) when the input waits
	// behind a turn someone else is running.
	DeliveredQueued SessionInputDelivery = "queued"
)

// SendSessionInputOptions configures SendSessionInput.
type SendSessionInputOptions struct {
	// Content is the ordered content blocks (text, images, documents, …) for
	// the input. Required. With DeliverCurrentTurn every block must be text.
	Content []map[string]interface{}
	// Delivery picks whether the input starts a new turn (the default) or
	// steers the running one.
	Delivery SessionInputDelivery
	// Context is the ordered application-owned state for a new turn.
	Context []RuntimeContextItem
	// IdempotencyKey dedupes the call within the session: a repeat returns the
	// original result and writes nothing new. Derive it from the provider
	// event id for webhook retries. The SDK retries the request automatically
	// only when a key is set.
	IdempotencyKey string
	// Operation configures policy for only a newly admitted turn. Its timeout
	// takes precedence over the stored agent timeout and is not saved.
	Operation *api.AgentTurnOperationPolicy
	// Output attaches a structured-output contract to a new turn. Mobius
	// exposes a reserved submit tool for the schema, validates the submission
	// server-side, and fails the turn if it never produces a schema-valid
	// object. Read the validated value from TurnTranscript.Output.
	Output *api.TurnOutputSpec
	// Metadata is free-form caller metadata attached to the input.
	Metadata map[string]interface{}
	// Wake, with DeliverCurrentTurn, interrupts a turn waiting on an
	// interruptible tool so it absorbs the direction now.
	Wake bool
}

// SessionInput reports where SendSessionInput put the input.
type SessionInput struct {
	// Delivery is DeliverNewTurn, DeliverCurrentTurn, or DeliveredQueued.
	Delivery SessionInputDelivery
	// SessionID is the session the input went to.
	SessionID string
	// TurnID is the new turn carrying the input, the turn direction is queued
	// for, or the running turn it waits behind.
	TurnID string
	// Deduped is true when a repeated idempotency key returned the original
	// result without writing again.
	Deduped bool
	// WokeTurn is true when Wake interrupted a waiting tool.
	WokeTurn bool
	// Nudge is the queued direction, present for DeliverCurrentTurn
	// requests. Cancel it with CancelNudge while it is pending.
	Nudge *api.SessionNudge
	// Turn is the started turn and its lazy live transcript. It is set
	// exactly when Delivery is DeliverNewTurn.
	Turn *TurnTranscript
}

// ListSessionsOptions configures ListSessions.
type ListSessionsOptions struct {
	AgentID    string
	AgentName  string
	SessionKey string
	Status     string
	// Q is a full-text search over titles and message text. Matching
	// responses carry Snippets.
	Q             string
	Scope         string
	Provider      string
	IntegrationID string
	Since         *time.Time
	Cursor        string
	Limit         int
}

// ListSessionMessagesOptions configures ListSessionMessages.
type ListSessionMessagesOptions struct {
	AfterSequence  int64
	BeforeSequence int64
	Order          string
	Limit          int
	// Include accepts "context" to return caller-supplied runtime context rows.
	Include string
}

// ListSessionNudgesOptions configures ListSessionNudges.
type ListSessionNudgesOptions struct {
	Statuses []string
	Order    string
	Cursor   string
	Limit    int
}

// ListSessionTurnsOptions configures ListSessionTurns.
type ListSessionTurnsOptions struct {
	IDs    []string
	Order  string
	Cursor string
	Limit  int
}

// CreateSession resolves or creates a durable session for an agent. With the
// default continue_or_create mode, a repeat call with the same SessionKey
// returns the existing session. A product backend calls it per inbound
// conversation, then sends each message with SendSessionInput.
func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (*api.Session, error) {
	if strings.TrimSpace(req.AgentId) == "" {
		return nil, errors.New("mobius: create session: AgentId is required")
	}
	resp, err := c.ac.CreateSessionWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create session: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("create session", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// SendSessionInput puts caller input into a session. By default it appends
// the input as a user message and starts an agent turn; the returned
// SessionInput.Turn carries the turn's identity immediately and its live
// transcript on demand (iterate with Next/Err to render the turn as it runs,
// or never iterate for fire-and-forget). With DeliverCurrentTurn the input
// steers the running turn instead and SessionInput.Nudge tracks it; when the
// session is idle the server starts a new turn and Turn is set.
func (c *Client) SendSessionInput(ctx context.Context, sessionID string, opts SendSessionInputOptions) (*SessionInput, error) {
	if len(opts.Content) == 0 {
		return nil, errors.New("mobius: send session input: content is required")
	}
	body := api.SessionInputRequest{Content: opts.Content}
	if opts.Delivery != "" {
		delivery := api.SessionInputDelivery(opts.Delivery)
		body.Delivery = &delivery
	}
	if opts.Context != nil {
		runtimeContext := api.RuntimeContext(opts.Context)
		body.Context = &runtimeContext
	}
	key := normalizeIdempotencyKey(opts.IdempotencyKey)
	body.IdempotencyKey = stringPointer(key)
	body.Operation = opts.Operation
	body.Output = opts.Output
	if opts.Metadata != nil {
		body.Metadata = &opts.Metadata
	}
	if opts.Wake {
		body.Wake = &opts.Wake
	}
	resp, err := c.ac.SendSessionInputWithResponse(ctx, api.SessionIdParam(sessionID), body, idempotencyRequestEditors(key)...)
	if err != nil {
		return nil, fmt.Errorf("mobius: send session input: %w", err)
	}
	if resp.JSON202 == nil {
		return nil, unexpectedSessionStatus("send session input", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return c.sessionInput(ctx, resp.JSON202)
}

func (c *Client) sessionInput(ctx context.Context, ack *api.SessionInputAck) (*SessionInput, error) {
	in := &SessionInput{
		Delivery:  SessionInputDelivery(ack.Delivery),
		SessionID: ack.Session.Id,
		TurnID:    ack.Turn.Id,
		Deduped:   ack.Deduped,
		WokeTurn:  ack.WokeTurn,
		Nudge:     ack.Nudge,
	}
	if ack.Delivery != api.SessionInputOutcomeNewTurn {
		return in, nil
	}
	turn, err := c.turnTranscript(ctx, "send session input", ack)
	if err != nil {
		return nil, err
	}
	in.Turn = turn
	return in, nil
}

func (c *Client) turnTranscript(ctx context.Context, op string, ack *api.SessionInputAck) (*TurnTranscript, error) {
	if ack.ResumeCursor == nil || strings.TrimSpace(*ack.ResumeCursor) == "" {
		return nil, fmt.Errorf("mobius: %s response missing resume_cursor", op)
	}
	view := NewSessionTranscript()
	view.Seed(ack)
	return &TurnTranscript{
		stream: transcriptStream{
			client:     c,
			ctx:        ctx,
			sessionID:  ack.Session.Id,
			stopTurnID: ack.Turn.Id,
			delay:      defaultTranscriptReconnectDelay,
			view:       view,
			connection: TranscriptConnectionIdle,
		},
		turnID:           ack.Turn.Id,
		sessionID:        ack.Session.Id,
		afterSequence:    ack.AfterSequence,
		deduped:          ack.Deduped,
		invocationCursor: *ack.ResumeCursor,
		hydrate:          IsTerminalTurnStatus(string(ack.Turn.Status)),
	}, nil
}

// ListSessions returns a cursor-paginated org session page.
func (c *Client) ListSessions(ctx context.Context, opts *ListSessionsOptions) (*api.SessionListResponse, error) {
	params := &api.ListSessionsParams{}
	if opts != nil {
		params.AgentId = stringPointer(opts.AgentID)
		params.AgentName = stringPointer(opts.AgentName)
		params.SessionKey = stringPointer(opts.SessionKey)
		if opts.Status != "" {
			v := api.SessionStatus(opts.Status)
			params.Status = &v
		}
		if opts.Scope != "" {
			v := api.SessionScope(opts.Scope)
			params.Scope = &v
		}
		params.Q = stringPointer(opts.Q)
		params.Provider = stringPointer(opts.Provider)
		params.IntegrationId = stringPointer(opts.IntegrationID)
		params.Since = opts.Since
		if opts.Cursor != "" {
			v := api.CursorParam(opts.Cursor)
			params.Cursor = &v
		}
		if opts.Limit > 0 {
			v := api.LimitParam(opts.Limit)
			params.Limit = &v
		}
	}
	resp, err := c.ac.ListSessionsWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list sessions: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("list sessions", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// GetSession returns one durable session.
func (c *Client) GetSession(ctx context.Context, sessionID string) (*api.Session, error) {
	resp, err := c.ac.GetSessionWithResponse(ctx, api.SessionIdParam(sessionID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get session: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("get session", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CancelSession cancels the session's active turn.
func (c *Client) CancelSession(ctx context.Context, sessionID string) (*api.Session, error) {
	resp, err := c.ac.CancelSessionWithResponse(
		ctx,
		api.SessionIdParam(sessionID),
		api.CancelSessionJSONRequestBody{},
	)
	if err != nil {
		return nil, fmt.Errorf("mobius: cancel session: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("cancel session", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CompactSession queues a compaction turn that summarizes the transcript since
// the last compaction. It returns the session as it was when the turn was
// queued; watch the session's turns for progress.
func (c *Client) CompactSession(ctx context.Context, sessionID string) (*api.Session, error) {
	resp, err := c.ac.CompactSessionWithResponse(ctx, api.SessionIdParam(sessionID))
	if err != nil {
		return nil, fmt.Errorf("mobius: compact session: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("compact session", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ListSessionMessages returns durable transcript rows.
func (c *Client) ListSessionMessages(ctx context.Context, sessionID string, opts *ListSessionMessagesOptions) (*api.SessionMessageListResponse, error) {
	params := &api.ListSessionMessagesParams{}
	if opts != nil {
		if opts.AfterSequence > 0 {
			v := api.AfterSequenceParam(opts.AfterSequence)
			params.AfterSequence = &v
		}
		if opts.BeforeSequence > 0 {
			v := api.BeforeSequenceParam(opts.BeforeSequence)
			params.BeforeSequence = &v
		}
		if opts.Order != "" {
			v := api.ListSessionMessagesParamsOrder(opts.Order)
			params.Order = &v
		}
		if opts.Limit > 0 {
			v := api.LimitParam(opts.Limit)
			params.Limit = &v
		}
		if opts.Include != "" {
			v := api.ContextIncludeParam(opts.Include)
			params.Include = &v
		}
	}
	resp, err := c.ac.ListSessionMessagesWithResponse(ctx, api.SessionIdParam(sessionID), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list session messages: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("list session messages", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ListSessionNudges returns the authoritative durable nudge queue.
func (c *Client) ListSessionNudges(ctx context.Context, sessionID string, opts *ListSessionNudgesOptions) (*api.SessionNudgeListResponse, error) {
	params := &api.ListSessionNudgesParams{}
	if opts != nil {
		if len(opts.Statuses) > 0 {
			statuses := make([]api.SessionNudgeStatus, len(opts.Statuses))
			for i, status := range opts.Statuses {
				statuses[i] = api.SessionNudgeStatus(status)
			}
			params.Status = &statuses
		}
		if opts.Order != "" {
			v := api.ListSessionNudgesParamsOrder(opts.Order)
			params.Order = &v
		}
		if opts.Cursor != "" {
			v := api.CursorParam(opts.Cursor)
			params.Cursor = &v
		}
		if opts.Limit > 0 {
			v := api.LimitParam(opts.Limit)
			params.Limit = &v
		}
	}
	resp, err := c.ac.ListSessionNudgesWithResponse(ctx, api.SessionIdParam(sessionID), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list session nudges: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("list session nudges", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// GetSessionNudge returns one durable nudge.
func (c *Client) GetSessionNudge(ctx context.Context, sessionID, nudgeID string) (*api.SessionNudge, error) {
	resp, err := c.ac.GetSessionNudgeWithResponse(ctx, api.SessionIdParam(sessionID), api.NudgeIdParam(nudgeID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get session nudge: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("get session nudge", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CancelNudge cancels a pending durable nudge.
func (c *Client) CancelNudge(ctx context.Context, sessionID, nudgeID string) (*api.SessionNudge, error) {
	resp, err := c.ac.CancelNudgeWithResponse(ctx, api.SessionIdParam(sessionID), api.NudgeIdParam(nudgeID))
	if err != nil {
		return nil, fmt.Errorf("mobius: cancel nudge: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("cancel nudge", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ListSessionTurns returns the turns attached to a session.
func (c *Client) ListSessionTurns(ctx context.Context, sessionID string, opts *ListSessionTurnsOptions) (*api.AgentTurnListResponse, error) {
	params := &api.ListSessionTurnsParams{}
	if opts != nil {
		if len(opts.IDs) > 0 {
			params.Ids = &opts.IDs
		}
		if opts.Order != "" {
			v := api.ListSessionTurnsParamsOrder(opts.Order)
			params.Order = &v
		}
		if opts.Cursor != "" {
			v := api.CursorParam(opts.Cursor)
			params.Cursor = &v
		}
		if opts.Limit > 0 {
			v := api.LimitParam(opts.Limit)
			params.Limit = &v
		}
	}
	resp, err := c.ac.ListSessionTurnsWithResponse(ctx, api.SessionIdParam(sessionID), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list session turns: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("list session turns", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// GetSessionTurn returns one session turn.
func (c *Client) GetSessionTurn(ctx context.Context, sessionID, turnID string) (*api.AgentTurn, error) {
	resp, err := c.ac.GetSessionTurnWithResponse(ctx, api.SessionIdParam(sessionID), api.TurnIdParam(turnID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get session turn: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("get session turn", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CancelTurn cancels one queued/running/waiting session turn.
func (c *Client) CancelTurn(ctx context.Context, sessionID, turnID string) (*api.AgentTurn, error) {
	resp, err := c.ac.CancelTurnWithResponse(ctx, api.SessionIdParam(sessionID), api.TurnIdParam(turnID))
	if err != nil {
		return nil, fmt.Errorf("mobius: cancel turn: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("cancel turn", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// DeleteSessionTurn takes back the session's last turn: its input, the
// agent's work, and its reply leave the transcript. Only the last turn of an
// idle session can be deleted (409 otherwise); repeat to walk further back.
// Deleting an already-deleted turn succeeds.
func (c *Client) DeleteSessionTurn(ctx context.Context, sessionID, turnID string) error {
	resp, err := c.ac.DeleteSessionTurnWithResponse(ctx, api.SessionIdParam(sessionID), api.TurnIdParam(turnID))
	if err != nil {
		return fmt.Errorf("mobius: delete session turn: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedSessionStatus("delete session turn", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return nil
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func unexpectedSessionStatus(op string, statusCode int, status string, response *http.Response, body []byte) error {
	header := http.Header{}
	if response != nil {
		header = response.Header
	}
	return unexpectedAPIStatus(op, statusCode, status, header, body)
}
