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
		return nil, unexpectedSessionStatus("invoke agent", resp.Status(), resp.Body)
	}
	ack := resp.JSON202
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
		},
		turnID:        ack.Turn.Id,
		sessionID:     ack.Session.Id,
		afterSequence: ack.AfterSequence,
		deduped:       ack.Deduped != nil && *ack.Deduped,
		hydrate:       IsTerminalTurnStatus(string(ack.Turn.Status)),
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
		_ = resp.Body.Close()
		return nil, fmt.Errorf("mobius: invoke agent stream: unexpected status %d", resp.StatusCode)
	}

	ch := make(chan SessionStreamEvent)
	go c.readSessionStream(ctx, resp.Body, ch)
	return ch, nil
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

func unexpectedSessionStatus(op, status string, body []byte) error {
	if len(body) > 0 {
		return fmt.Errorf("mobius: %s: unexpected status %s: %s", op, status, string(body))
	}
	return fmt.Errorf("mobius: %s: unexpected status %s", op, status)
}
