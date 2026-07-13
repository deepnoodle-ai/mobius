package mobius

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/sse"
)

// InvokeAgentOptions configures InvokeAgent and InvokeAgentStream. Provide
// exactly one of AgentID or AgentName to select the agent, and Content for
// the caller's input message.
type InvokeAgentOptions struct {
	// AgentID is the agent identifier. Mutually exclusive with AgentName.
	AgentID string
	// AgentName is the project-unique agent name. Mutually exclusive with
	// AgentID.
	AgentName string

	// Content is the ordered content blocks (text, images, …) for the
	// caller's input message. Required.
	Content []map[string]interface{}
	// Context is the ordered application-owned state for this turn.
	Context []RuntimeContextItem
	// IdempotencyKey dedupes the call: a repeat call with the same key
	// resolves the same session and resumes the existing turn rather than
	// starting a second one. Derive it from the provider event id for
	// Slack/Telegram webhook retries.
	IdempotencyKey string
	// InputMetadata is free-form caller metadata attached to the input
	// message.
	InputMetadata map[string]interface{}

	// Session controls how the session this turn runs in is resolved or
	// created. Omit to use a single default session per agent in
	// continue_or_create mode. Set Session.ThinkingEffort to override the
	// agent's reasoning-effort default for this session.
	Session *api.InvokeSessionSpec
	// Config sends an inline agent definition (instructions, model, effort,
	// timeout, toolkits, skills) with the invocation instead of using the
	// agent stored in Mobius. Set fields replace the agent's values; omitted
	// fields keep them. Mobius remembers the config on the session and reuses
	// it on later turns until a new one is sent. Omit to run the agent on its
	// stored definition.
	Config *api.InlineAgentConfig
	// ChannelContext records optional messaging provider/channel routing
	// context (Slack, Telegram, …) on the started turn.
	ChannelContext *api.ChannelContext
}

// StartTurnOptions configures StartTurn for an existing session.
type StartTurnOptions struct {
	// Content is the ordered content blocks (text, images, …) for the
	// caller's input message. Required.
	Content []map[string]interface{}
	// Context is the ordered application-owned state for this turn.
	Context []RuntimeContextItem
	// IdempotencyKey dedupes the call within the session.
	IdempotencyKey string
	// Metadata is free-form caller metadata attached to the input message.
	Metadata map[string]interface{}
}

// SessionStreamEvent is a single decoded frame from a session SSE stream.
// EventType is the authoritative SSE event: name (e.g. "user.message",
// "turn.completed", "tool.call") — decode Frame with the matching
// api.SessionStreamFrame AsXxxPayload accessor. The union is reference-only
// and cannot be shape-matched from the payload alone, per
// api.SessionStreamFrame's doc.
type SessionStreamEvent struct {
	EventType string
	Frame     api.SessionStreamFrame
}

// ListSessionsOptions configures ListSessions.
type ListSessionsOptions struct {
	AgentID       string
	SessionKey    string
	Status        string
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

// NudgeSessionOptions is explicit mid-turn user direction. The same
// IdempotencyKey and Content dedupe; Wake may interrupt a waiting tool.
type NudgeSessionOptions struct {
	Content        string
	IdempotencyKey string
	Metadata       map[string]interface{}
	Wake           bool
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

// InvokeAgent resolves (or creates) a session, appends Content as the
// caller's input message, and starts an agent turn — collapsing the
// create-or-resolve-session + start-turn sequence into one retryable call.
// This is the entry point for a product backend (an embedded app, a Slack
// handler, a Telegram bot) calling per inbound message; use the lower-level
// session and turn operations on RawClient for finer control.
//
// It returns once the turn is accepted. The returned TurnTranscript carries
// the turn's identity (ID, SessionID, Status) immediately and its live
// transcript on demand: the stream is lazy, so iterate with Next/Err to
// render the turn as it runs, or never iterate for fire-and-forget. Use
// InvokeAgentStream instead to observe the turn's activity inline on the
// same connection with v1 session-stream framing.
func (c *Client) InvokeAgent(ctx context.Context, opts InvokeAgentOptions) (*TurnTranscript, error) {
	req, err := invokeAgentRequest(opts)
	if err != nil {
		return nil, err
	}
	resp, err := c.ac.InvokeAgentWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: invoke agent: %w", err)
	}
	if resp.JSON202 == nil {
		return nil, unexpectedSessionStatus("invoke agent", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return c.turnTranscript(ctx, "invoke agent", resp.JSON202)
}

// StartTurn appends Content to an existing session and starts an agent turn.
// Like InvokeAgent, it returns once the turn is accepted and exposes the live
// transcript lazily through the returned handle.
func (c *Client) StartTurn(ctx context.Context, sessionID string, opts StartTurnOptions) (*TurnTranscript, error) {
	if len(opts.Content) == 0 {
		return nil, errors.New("mobius: start turn: content is required")
	}
	body := api.StartTurnRequest{Content: opts.Content}
	if opts.Context != nil {
		runtimeContext := api.RuntimeContext(opts.Context)
		body.Context = &runtimeContext
	}
	body.IdempotencyKey = stringPointer(opts.IdempotencyKey)
	if opts.Metadata != nil {
		body.Metadata = &opts.Metadata
	}
	resp, err := c.ac.StartTurnWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), body)
	if err != nil {
		return nil, fmt.Errorf("mobius: start turn: %w", err)
	}
	if resp.JSON202 == nil {
		return nil, unexpectedSessionStatus("start turn", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return c.turnTranscript(ctx, "start turn", resp.JSON202)
}

func (c *Client) turnTranscript(ctx context.Context, op string, ack *api.TurnAck) (*TurnTranscript, error) {
	if strings.TrimSpace(ack.ResumeCursor) == "" {
		return nil, fmt.Errorf("mobius: %s response missing resume_cursor", op)
	}
	view := NewSessionTranscript()
	view.Seed(ack)
	deduped := ack.Deduped != nil && *ack.Deduped
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
		deduped:          deduped,
		invocationCursor: ack.ResumeCursor,
		hydrate:          IsTerminalTurnStatus(string(ack.Turn.Status)),
	}, nil
}

// InvokeAgentStream behaves like InvokeAgent but streams the turn's activity
// inline on the same connection instead of waiting for a TurnAck, identical
// to framing from GET …/sessions/{id}/stream. The channel is closed when ctx
// is cancelled or the server closes the connection.
func (c *Client) InvokeAgentStream(ctx context.Context, opts InvokeAgentOptions) (<-chan SessionStreamEvent, error) {
	req, err := invokeAgentRequest(opts)
	if err != nil {
		return nil, err
	}
	resp, err := c.ac.InvokeAgent(ctx, api.ProjectHandleParam(c.projectHandle), req, acceptEventStream)
	if err != nil {
		return nil, fmt.Errorf("mobius: invoke agent stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, unexpectedAPIStatus("invoke agent stream", resp.StatusCode, resp.Status, resp.Header, body)
	}

	ch := make(chan SessionStreamEvent)
	go c.readSessionStream(ctx, resp.Body, ch)
	return ch, nil
}

// ListSessions returns a cursor-paginated project session page.
func (c *Client) ListSessions(ctx context.Context, opts *ListSessionsOptions) (*api.SessionListResponse, error) {
	params := &api.ListSessionsParams{}
	if opts != nil {
		params.AgentId = stringPointer(opts.AgentID)
		params.SessionKey = stringPointer(opts.SessionKey)
		if opts.Status != "" {
			v := api.SessionStatus(opts.Status)
			params.Status = &v
		}
		if opts.Scope != "" {
			v := api.SessionScope(opts.Scope)
			params.Scope = &v
		}
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
	resp, err := c.ac.ListSessionsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), params)
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
	resp, err := c.ac.GetSessionWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get session: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("get session", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CancelSession cancels the active direct turn. Force additionally cancels
// loop-owned turns and should be reserved for recovery.
func (c *Client) CancelSession(ctx context.Context, sessionID string, force bool) (*api.Session, error) {
	params := &api.CancelSessionParams{}
	if force {
		params.Force = &force
	}
	resp, err := c.ac.CancelSessionWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: cancel session: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("cancel session", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// CompactSession requests manual compaction of a session transcript.
func (c *Client) CompactSession(ctx context.Context, sessionID string) (*api.Session, error) {
	resp, err := c.ac.CompactSessionWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID))
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
	resp, err := c.ac.ListSessionMessagesWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list session messages: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("list session messages", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// NudgeSession sends explicit direction to an in-flight turn, or queues a
// follow-up turn when the terminal race wins.
func (c *Client) NudgeSession(ctx context.Context, sessionID string, opts NudgeSessionOptions) (*api.SessionNudgeAck, error) {
	body := api.NudgeSessionRequest{Content: opts.Content}
	body.IdempotencyKey = stringPointer(opts.IdempotencyKey)
	if opts.Metadata != nil {
		body.Metadata = &opts.Metadata
	}
	if opts.Wake {
		body.Wake = &opts.Wake
	}
	resp, err := c.ac.NudgeSessionWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), body)
	if err != nil {
		return nil, fmt.Errorf("mobius: nudge session: %w", err)
	}
	if resp.JSON202 == nil {
		return nil, unexpectedSessionStatus("nudge session", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON202, nil
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
	resp, err := c.ac.ListSessionNudgesWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), params)
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
	resp, err := c.ac.GetSessionNudgeWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), api.NudgeIdParam(nudgeID))
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
	resp, err := c.ac.CancelNudgeWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), api.NudgeIdParam(nudgeID))
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
	resp, err := c.ac.ListSessionTurnsWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), params)
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
	resp, err := c.ac.GetSessionTurnWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), api.TurnIdParam(turnID))
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
	resp, err := c.ac.CancelTurnWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.SessionIdParam(sessionID), api.TurnIdParam(turnID))
	if err != nil {
		return nil, fmt.Errorf("mobius: cancel turn: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedSessionStatus("cancel turn", resp.StatusCode(), resp.Status(), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// readSessionStream decodes SSE frames from body using wonton/sse and
// forwards them on ch. The body is closed when the stream ends or ctx is
// cancelled.
func (c *Client) readSessionStream(ctx context.Context, body io.ReadCloser, ch chan<- SessionStreamEvent) {
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
		if evt.Event == "" {
			continue
		}

		var frame api.SessionStreamFrame
		if err := frame.UnmarshalJSON([]byte(evt.Data)); err != nil {
			c.config.Logger.Error("failed to parse SSE event", "error", err)
			continue
		}

		select {
		case ch <- SessionStreamEvent{EventType: evt.Event, Frame: frame}:
		case <-ctx.Done():
			return
		}
	}
}

func invokeAgentRequest(opts InvokeAgentOptions) (api.InvokeAgentRequest, error) {
	if opts.AgentID == "" && opts.AgentName == "" {
		return api.InvokeAgentRequest{}, errors.New("mobius: invoke agent: AgentID or AgentName is required")
	}
	if len(opts.Content) == 0 {
		return api.InvokeAgentRequest{}, errors.New("mobius: invoke agent: content is required")
	}

	agentRef := api.AgentRef{}
	if opts.AgentID != "" {
		agentRef.Id = &opts.AgentID
	}
	if opts.AgentName != "" {
		agentRef.Name = &opts.AgentName
	}

	input := api.InvokeInput{Content: opts.Content}
	if opts.Context != nil {
		runtimeContext := api.RuntimeContext(opts.Context)
		input.Context = &runtimeContext
	}
	if opts.IdempotencyKey != "" {
		input.IdempotencyKey = &opts.IdempotencyKey
	}
	if opts.InputMetadata != nil {
		input.Metadata = &opts.InputMetadata
	}

	req := api.InvokeAgentRequest{AgentRef: agentRef, Input: input}
	if opts.Session != nil {
		req.Session = opts.Session
	}
	if opts.Config != nil {
		req.Config = opts.Config
	}
	if opts.ChannelContext != nil {
		req.ChannelContext = opts.ChannelContext
	}
	return req, nil
}

func unexpectedSessionStatus(op string, statusCode int, status string, response *http.Response, body []byte) error {
	header := http.Header{}
	if response != nil {
		header = response.Header
	}
	return unexpectedAPIStatus(op, statusCode, status, header, body)
}
