package mobius

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// Context is the action-facing extension of context.Context. It exposes
// identity fields about the currently executing job, a structured
// logger scoped to the job, and worker-side helpers for the runtime
// API endpoints that take effect under the current lease. All methods
// are safe for concurrent use.
type Context interface {
	context.Context

	Logger() *slog.Logger
	// ProjectHandle returns the project handle this job is running in
	// (e.g. "default"). The handle is the URL-safe identifier used in
	// project-scoped API routes; it is not the project's TypeID.
	ProjectHandle() string
	// ProjectID is a deprecated alias for ProjectHandle.
	//
	// Deprecated: use ProjectHandle. The name was misleading because
	// it returns the project handle, not the project's TypeID
	// (e.g. "prj_…"). Will be removed in a future release.
	ProjectID() string
	RunID() string
	JobID() string
	WorkflowName() string
	StepName() string
	Attempt() int
	Queue() string

	// EmitEvent enqueues a custom run event for delivery while the
	// worker holds the current job's lease. Events are batched and
	// flushed by the SDK; payload size is bounded server-side.
	EmitEvent(eventType string, payload map[string]any)

	// RunServerAction invokes a named server-side action while the
	// worker still holds this job's lease. The action must exist in
	// the project or platform action catalog (see
	// [Client.ListActionCatalog]). Returns the action's output
	// decoded as a Go value (typically map[string]any).
	RunServerAction(actionName string, params map[string]any, opts *RunServerActionOptions) (any, error)

	// RequestInteraction creates a job-scoped human-in-the-loop
	// interaction. The server derives the owning run from the job
	// and, when SignalName is empty, derives the resume signal name
	// from the step. Returns the created Interaction.
	RequestInteraction(req InteractionRequest) (*api.Interaction, error)
}

// RunServerActionOptions configures a [Context.RunServerAction] call.
// All fields are optional.
type RunServerActionOptions struct {
	// DryRun invokes the action without side effects when the action
	// supports it. Actions that do not support dry-run reject the call.
	DryRun bool
	// TimeoutSeconds overrides the default action execution timeout.
	// Zero means use the server default.
	TimeoutSeconds int
}

// InteractionTarget identifies who should receive an interaction. Use
// [InteractionTargetTypeUser], [InteractionTargetTypeAgent], or
// [InteractionTargetTypeGroup] for the Type field.
type InteractionTarget struct {
	Type InteractionTargetType
	ID   string
}

// InteractionTargetType is the kind of an [InteractionTarget].
type InteractionTargetType string

const (
	InteractionTargetTypeUser  InteractionTargetType = "user"
	InteractionTargetTypeAgent InteractionTargetType = "agent"
	InteractionTargetTypeGroup InteractionTargetType = "group"
)

// InteractionKind is the kind of an interaction request.
type InteractionKind string

const (
	// InteractionKindApproval captures a yes/no decision.
	InteractionKindApproval InteractionKind = "approval"
	// InteractionKindReview captures an acknowledgement or review notes.
	InteractionKindReview InteractionKind = "review"
	// InteractionKindInput collects free-form data from the responder.
	InteractionKindInput InteractionKind = "input"
)

// InteractionRequest is the input for [Context.RequestInteraction]. It
// mirrors the wire schema of CreateJobInteractionRequest but uses
// idiomatic Go types so callers do not need to pointer-wrap optional
// fields.
type InteractionRequest struct {
	// Target is the user, agent queue, or group that receives the
	// interaction. Required.
	Target InteractionTarget
	// Kind is the interaction type (approval / review / input). Required.
	Kind InteractionKind
	// Message is the human-readable prompt shown to the responder. Required.
	Message string
	// SignalName overrides the server-derived resume signal. Optional;
	// when empty the server derives a name from the step.
	SignalName string
	// Timeout is a duration string (e.g. "24h", "30m") after which
	// the interaction expires. Optional.
	Timeout string
	// Context is additional structured detail surfaced in the UI
	// alongside the message. Optional.
	Context map[string]any
	// Spec is the declarative dialog contract used to render and
	// validate the response. Optional; when nil the server applies
	// the default for the kind.
	Spec *api.InteractionSpec
	// RequireAll requires every snapshotted member of a group target
	// to respond. Ignored for non-group targets.
	RequireAll bool
}

type executionContext struct {
	context.Context
	client       *Client
	emit         func(string, map[string]any)
	logger       *slog.Logger
	projectHndl  string
	runID        string
	jobID        string
	workflowName string
	stepName     string
	attempt      int
	queue        string
}

func (c *executionContext) Logger() *slog.Logger    { return c.logger }
func (c *executionContext) ProjectHandle() string   { return c.projectHndl }
func (c *executionContext) ProjectID() string       { return c.projectHndl }
func (c *executionContext) RunID() string           { return c.runID }
func (c *executionContext) JobID() string           { return c.jobID }
func (c *executionContext) WorkflowName() string    { return c.workflowName }
func (c *executionContext) StepName() string        { return c.stepName }
func (c *executionContext) Attempt() int            { return c.attempt }
func (c *executionContext) Queue() string           { return c.queue }
func (c *executionContext) EmitEvent(eventType string, payload map[string]any) {
	if c.emit != nil {
		c.emit(eventType, payload)
	}
}

func (c *executionContext) RunServerAction(actionName string, params map[string]any, opts *RunServerActionOptions) (any, error) {
	if c.client == nil {
		return nil, fmt.Errorf("mobius: RunServerAction: context not bound to a client")
	}
	if actionName == "" {
		return nil, fmt.Errorf("mobius: RunServerAction: actionName is required")
	}
	body := api.RunJobActionJSONRequestBody{}
	if params != nil {
		p := params
		body.Parameters = &p
	}
	if opts != nil {
		if opts.DryRun {
			dr := true
			body.DryRun = &dr
		}
		if opts.TimeoutSeconds > 0 {
			ts := opts.TimeoutSeconds
			body.TimeoutSeconds = &ts
		}
	}
	resp, err := c.client.ac.RunJobActionWithResponse(c,
		api.ProjectHandleParam(c.projectHndl),
		api.IDParam(c.jobID),
		api.ActionNameParam(actionName),
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("mobius: RunServerAction: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("mobius: RunServerAction: unexpected status %s: %s", resp.Status(), string(resp.Body))
	}
	return decodeRunActionOutput(resp.JSON200.Output)
}

func (c *executionContext) RequestInteraction(req InteractionRequest) (*api.Interaction, error) {
	if c.client == nil {
		return nil, fmt.Errorf("mobius: RequestInteraction: context not bound to a client")
	}
	if req.Target.ID == "" {
		return nil, fmt.Errorf("mobius: RequestInteraction: target.ID is required")
	}
	if req.Target.Type == "" {
		return nil, fmt.Errorf("mobius: RequestInteraction: target.Type is required")
	}
	if req.Kind == "" {
		return nil, fmt.Errorf("mobius: RequestInteraction: Kind is required")
	}
	if req.Message == "" {
		return nil, fmt.Errorf("mobius: RequestInteraction: Message is required")
	}
	body := api.CreateJobInteractionJSONRequestBody{
		Target: api.InteractionTarget{
			Type: api.InteractionTargetType(req.Target.Type),
			Id:   req.Target.ID,
		},
		Type:    api.InteractionType(req.Kind),
		Message: req.Message,
	}
	if req.SignalName != "" {
		s := req.SignalName
		body.SignalName = &s
	}
	if req.Timeout != "" {
		t := req.Timeout
		body.Timeout = &t
	}
	if req.Context != nil {
		ctxMap := req.Context
		body.Context = &ctxMap
	}
	if req.Spec != nil {
		body.Spec = req.Spec
	}
	if req.RequireAll {
		ra := true
		body.RequireAll = &ra
	}
	if c.stepName != "" {
		s := c.stepName
		body.StepName = &s
	}
	resp, err := c.client.ac.CreateJobInteractionWithResponse(c,
		api.ProjectHandleParam(c.projectHndl),
		api.IDParam(c.jobID),
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("mobius: RequestInteraction: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, fmt.Errorf("mobius: RequestInteraction: unexpected status %s: %s", resp.Status(), string(resp.Body))
	}
	return resp.JSON201, nil
}

// decodeRunActionOutput unwraps a RunActionResult union into a Go
// value: object → map[string]any, array → []any, primitives as-is.
func decodeRunActionOutput(out api.RunActionResult_Output) (any, error) {
	if v, err := out.AsRunActionResultOutput0(); err == nil {
		return map[string]any(v), nil
	}
	if v, err := out.AsRunActionResultOutput1(); err == nil {
		return []any(v), nil
	}
	if v, err := out.AsRunActionResultOutput2(); err == nil {
		return v, nil
	}
	if v, err := out.AsRunActionResultOutput3(); err == nil {
		return v, nil
	}
	if v, err := out.AsRunActionResultOutput4(); err == nil {
		return v, nil
	}
	if v, err := out.AsRunActionResultOutput5(); err == nil {
		return v, nil
	}
	return nil, fmt.Errorf("mobius: RunServerAction: unrecognized output shape")
}

func newContext(ctx context.Context, client *Client, j *runtimeJob, logger *slog.Logger, emit func(string, map[string]any)) Context {
	return &executionContext{
		Context:      ctx,
		client:       client,
		emit:         emit,
		logger:       logger,
		projectHndl:  j.ProjectHandle,
		runID:        j.RunID,
		jobID:        j.JobID,
		workflowName: j.WorkflowName,
		stepName:     j.StepName,
		attempt:      j.Attempt,
		queue:        j.Queue,
	}
}
