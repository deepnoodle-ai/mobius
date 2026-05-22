package mobius

import (
	"context"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

const (
	defaultWaitDiscussionTimeout      = 24 * time.Hour
	defaultWaitDiscussionPollInterval = 5 * time.Second
)

// DiscussionsService exposes helpers for discussion-oriented channel flows.
type DiscussionsService struct {
	client *Client
}

// StartDiscussionOptions describes a channel discussion that should resolve
// one or more interactions.
type StartDiscussionOptions struct {
	// ChannelID attaches the discussion to an existing channel. When omitted,
	// Start creates a private purpose channel.
	ChannelID string

	// Name is the new channel handle. Required when ChannelID is empty.
	Name        string
	DisplayName string
	Topic       string
	Kind        api.CreateChannelRequestKind
	Private     *bool
	MemberIDs   []string
	Tags        map[string]string

	// AssociatedInteractionIDs links existing interactions to the discussion.
	AssociatedInteractionIDs []string
	// Interactions creates standalone interactions, links them to the
	// discussion, and rolls them back if setup fails before the opening message.
	Interactions []api.CreateStandaloneInteractionRequest

	CompletionBehavior *api.ChannelCompletionBehavior
	OpeningMessage     string
	Wait               *WaitDiscussionOptions
}

// WaitDiscussionOptions controls StartDiscussion's optional completion wait.
type WaitDiscussionOptions struct {
	Timeout      time.Duration
	PollInterval time.Duration
}

// StartDiscussionResult is the channel, opening message, and interaction state
// produced by StartDiscussion.
type StartDiscussionResult struct {
	ChannelID             string
	OpeningMessageID      string
	InteractionIDs        []string
	CreatedInteractionIDs []string
	Channel               *api.Channel
	OpeningMessage        *api.ChannelMessage
	Interactions          []*api.Interaction
	Outcomes              []DiscussionOutcome
}

// DiscussionOutcome snapshots a terminal interaction outcome.
type DiscussionOutcome struct {
	InteractionID       string
	Status              api.InteractionStatus
	Outcome             *api.InteractionValue
	Responder           *api.InteractionResponder
	Responses           *[]api.InteractionResponse
	ResolvedBy          *string
	ResolvingResponseID *string
	Interaction         *api.Interaction
}

// Discussions returns high-level helpers for channel discussion flows.
func (c *Client) Discussions() *DiscussionsService {
	return &DiscussionsService{client: c}
}

// StartDiscussion creates or reuses a channel, associates one or more
// interactions, posts an opening message, and optionally waits for all linked
// interactions to terminalize.
func (c *Client) StartDiscussion(ctx context.Context, opts StartDiscussionOptions) (*StartDiscussionResult, error) {
	return c.Discussions().Start(ctx, opts)
}

// Start creates or reuses a channel discussion around interactions.
func (s *DiscussionsService) Start(ctx context.Context, opts StartDiscussionOptions) (*StartDiscussionResult, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("mobius: discussions client is nil")
	}
	if opts.OpeningMessage == "" {
		return nil, fmt.Errorf("mobius: start discussion requires an opening message")
	}
	if len(opts.AssociatedInteractionIDs) == 0 && len(opts.Interactions) == 0 {
		return nil, fmt.Errorf("mobius: start discussion requires at least one interaction")
	}

	var createdInteractionIDs []string
	rollbackCreated := func() {
		s.cancelCreatedInteractions(context.Background(), createdInteractionIDs)
	}
	shouldRollback := true
	defer func() {
		if shouldRollback {
			rollbackCreated()
		}
	}()

	var interactions []*api.Interaction
	for _, interaction := range opts.Interactions {
		created, err := s.createInteraction(ctx, interaction)
		if err != nil {
			return nil, err
		}
		createdInteractionIDs = append(createdInteractionIDs, created.Id)
		interactions = append(interactions, created)
	}

	interactionIDs := uniqueDiscussionIDs(append(append([]string{}, opts.AssociatedInteractionIDs...), createdInteractionIDs...))
	if len(interactionIDs) == 0 {
		return nil, fmt.Errorf("mobius: start discussion requires at least one interaction or associated interaction ID")
	}

	var channel *api.Channel
	channelID := opts.ChannelID
	if channelID == "" {
		created, err := s.createDiscussionChannel(ctx, opts, interactionIDs)
		if err != nil {
			return nil, err
		}
		channel = created
		channelID = created.Id
	} else {
		for _, interactionID := range interactionIDs {
			if err := s.associateInteraction(ctx, channelID, interactionID); err != nil {
				return nil, err
			}
		}
	}

	message, err := s.sendOpeningMessage(ctx, channelID, opts.OpeningMessage, interactionIDs)
	if err != nil {
		return nil, err
	}
	shouldRollback = false

	result := &StartDiscussionResult{
		ChannelID:             channelID,
		InteractionIDs:        interactionIDs,
		CreatedInteractionIDs: createdInteractionIDs,
		Channel:               channel,
		OpeningMessage:        message,
		Interactions:          interactions,
	}
	if message != nil {
		result.OpeningMessageID = message.Id
	}

	if opts.Wait != nil {
		finalInteractions, err := s.waitInteractions(ctx, interactionIDs, opts.Wait)
		result.Interactions = finalInteractions
		result.Outcomes = discussionOutcomes(finalInteractions)
		if err != nil {
			return result, err
		}
	}

	return result, nil
}

func (s *DiscussionsService) createDiscussionChannel(ctx context.Context, opts StartDiscussionOptions, associatedInteractionIDs []string) (*api.Channel, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("mobius: start discussion requires a channel name when ChannelID is empty")
	}
	displayName := opts.DisplayName
	if displayName == "" {
		displayName = opts.Name
	}
	kind := opts.Kind
	if kind == "" {
		kind = api.CreateChannelRequestKindChannel
	}
	private := true
	if opts.Private != nil {
		private = *opts.Private
	}
	purpose := api.CreateChannelRequestPurposeResolveInteractions
	req := api.CreateChannelRequest{
		Name:                     opts.Name,
		DisplayName:              displayName,
		Kind:                     kind,
		Private:                  &private,
		Purpose:                  &purpose,
		AssociatedInteractionIds: stringSlicePtr(associatedInteractionIDs),
		MemberIds:                stringSlicePtr(opts.MemberIDs),
		CompletionBehavior:       opts.CompletionBehavior,
		Tags:                     tagMapPtr(opts.Tags),
		Topic:                    stringPtrIfNotEmpty(opts.Topic),
	}
	resp, err := s.client.ac.CreateChannelWithResponse(ctx, api.ProjectHandleParam(s.client.projectHandle), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: create discussion channel: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedRunStatus("create discussion channel", resp.Status(), resp.Body)
	}
	return resp.JSON201, nil
}

func (s *DiscussionsService) createInteraction(ctx context.Context, interaction api.CreateStandaloneInteractionRequest) (*api.Interaction, error) {
	var body api.CreateInteractionRequest
	if err := body.FromCreateStandaloneInteractionRequest(interaction); err != nil {
		return nil, fmt.Errorf("mobius: create discussion interaction request: %w", err)
	}
	resp, err := s.client.ac.CreateInteractionWithResponse(ctx, api.ProjectHandleParam(s.client.projectHandle), body)
	if err != nil {
		return nil, fmt.Errorf("mobius: create discussion interaction: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedRunStatus("create discussion interaction", resp.Status(), resp.Body)
	}
	return resp.JSON201, nil
}

func (s *DiscussionsService) associateInteraction(ctx context.Context, channelID, interactionID string) error {
	relation := api.AssociateChannelInteractionRequestRelation(api.ChannelInteractionLinkRelationPurpose)
	resp, err := s.client.ac.AssociateChannelInteractionWithResponse(ctx, api.ProjectHandleParam(s.client.projectHandle), api.IDParam(channelID), api.AssociateChannelInteractionRequest{
		InteractionId: interactionID,
		Relation:      &relation,
	})
	if err != nil {
		return fmt.Errorf("mobius: associate discussion interaction: %w", err)
	}
	if resp.JSON201 == nil {
		return unexpectedRunStatus("associate discussion interaction", resp.Status(), resp.Body)
	}
	return nil
}

func (s *DiscussionsService) sendOpeningMessage(ctx context.Context, channelID, content string, interactionIDs []string) (*api.ChannelMessage, error) {
	messageType := "user.message"
	metadata := api.Metadata{
		"mobius_helper": "discussions.start",
	}
	refs := make([]api.EntityReference, 0, len(interactionIDs))
	relation := "purpose"
	for _, interactionID := range interactionIDs {
		refs = append(refs, api.EntityReference{
			EntityType: api.EntityReferenceTypeInteraction,
			EntityId:   interactionID,
			Relation:   &relation,
		})
	}
	req := api.SendChannelMessageRequest{
		Content:    &content,
		Metadata:   &metadata,
		References: &refs,
		Type:       &messageType,
	}
	resp, err := s.client.ac.SendMessageWithResponse(ctx, api.ProjectHandleParam(s.client.projectHandle), api.IDParam(channelID), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: send discussion opening message: %w", err)
	}
	if resp.JSON201 == nil {
		return nil, unexpectedRunStatus("send discussion opening message", resp.Status(), resp.Body)
	}
	return resp.JSON201, nil
}

func (s *DiscussionsService) waitInteractions(ctx context.Context, ids []string, opts *WaitDiscussionOptions) ([]*api.Interaction, error) {
	timeout := defaultWaitDiscussionTimeout
	poll := defaultWaitDiscussionPollInterval
	if opts != nil {
		if opts.Timeout > 0 {
			timeout = opts.Timeout
		}
		if opts.PollInterval > 0 {
			poll = opts.PollInterval
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		interactions := make([]*api.Interaction, 0, len(ids))
		allTerminal := true
		for _, id := range ids {
			resp, err := s.client.ac.GetInteractionWithResponse(waitCtx, api.ProjectHandleParam(s.client.projectHandle), api.IDParam(id))
			if err != nil {
				return nil, fmt.Errorf("mobius: get discussion interaction: %w", err)
			}
			if resp.JSON200 == nil {
				return nil, unexpectedRunStatus("get discussion interaction", resp.Status(), resp.Body)
			}
			interactions = append(interactions, resp.JSON200)
			if !isTerminalInteractionStatus(resp.JSON200.Status) {
				allTerminal = false
			}
		}
		if allTerminal {
			return interactions, nil
		}
		if err := sleepContext(waitCtx, poll); err != nil {
			return interactions, fmt.Errorf("mobius: timed out waiting for discussion interactions: %w", err)
		}
	}
}

func (s *DiscussionsService) cancelCreatedInteractions(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	reason := "discussion_start_failed"
	for _, id := range ids {
		_, _ = s.client.ac.CancelInteractionWithResponse(ctx, api.ProjectHandleParam(s.client.projectHandle), api.IDParam(id), api.CancelInteractionRequest{
			Reason: &reason,
		})
	}
}

func isTerminalInteractionStatus(status api.InteractionStatus) bool {
	switch status {
	case api.InteractionStatusCompleted, api.InteractionStatusExpired, api.InteractionStatusCancelled:
		return true
	default:
		return false
	}
}

func discussionOutcomes(interactions []*api.Interaction) []DiscussionOutcome {
	outcomes := make([]DiscussionOutcome, 0, len(interactions))
	for _, interaction := range interactions {
		if interaction == nil {
			continue
		}
		outcomes = append(outcomes, DiscussionOutcome{
			InteractionID:       interaction.Id,
			Status:              interaction.Status,
			Outcome:             interaction.Outcome,
			Responder:           interaction.Responder,
			Responses:           interaction.Responses,
			ResolvedBy:          interaction.ResolvedBy,
			ResolvingResponseID: interaction.ResolvingResponseId,
			Interaction:         interaction,
		})
	}
	return outcomes
}

func uniqueDiscussionIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func stringSlicePtr(values []string) *[]string {
	if len(values) == 0 {
		return nil
	}
	return &values
}

func stringPtrIfNotEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func tagMapPtr(tags map[string]string) *api.TagMap {
	if len(tags) == 0 {
		return nil
	}
	tagMap := api.TagMap(tags)
	return &tagMap
}
