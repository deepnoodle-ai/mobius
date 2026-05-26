package mobius

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// WebhookEventTypeHeader mirrors the webhook event type for receivers
	// that want to route before decoding the JSON body.
	WebhookEventTypeHeader = "X-Mobius-Event-Type"
)

// Common Mobius outgoing webhook event types.
const (
	WebhookEventRunCompleted WebhookEventType = "run.completed"
	WebhookEventRunFailed    WebhookEventType = "run.failed"
	WebhookEventPing         WebhookEventType = "ping"
)

// WebhookEventType identifies the kind of outgoing webhook delivery.
type WebhookEventType string

// WebhookEvent is the generic Mobius outgoing webhook envelope.
type WebhookEvent struct {
	Type WebhookEventType `json:"type"`
	Data json.RawMessage  `json:"data"`
}

// parseWebhookEvent decodes a Mobius outgoing webhook envelope. The Data field
// remains raw JSON so callers can unmarshal it into the event-specific type
// they care about.
func parseWebhookEvent(body []byte) (*WebhookEvent, error) {
	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("mobius: parse webhook event: %w", err)
	}
	if event.Type == "" {
		return nil, errors.New("mobius: parse webhook event: missing type")
	}
	return &event, nil
}
