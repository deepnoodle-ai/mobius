package mobius

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// WebhookSignatureHeader is the header Mobius uses for outgoing webhook
	// HMAC signatures.
	WebhookSignatureHeader = "X-Mobius-Signature"
	// WebhookEventTypeHeader mirrors the webhook event type for receivers
	// that want to route before decoding the JSON body.
	WebhookEventTypeHeader = "X-Mobius-Event-Type"

	webhookSignaturePrefix = "sha256="
)

// Common Mobius outgoing webhook event types.
const (
	WebhookEventRunCompleted WebhookEventType = "run.completed"
	WebhookEventRunFailed    WebhookEventType = "run.failed"
	WebhookEventPing         WebhookEventType = "ping"
)

var (
	// ErrInvalidWebhookSignature is returned when a webhook signature is
	// missing, malformed, or does not match the request body.
	ErrInvalidWebhookSignature = errors.New("mobius: invalid webhook signature")
)

// WebhookEventType identifies the kind of outgoing webhook delivery.
type WebhookEventType string

// WebhookEvent is the generic Mobius outgoing webhook envelope.
type WebhookEvent struct {
	Type WebhookEventType `json:"type"`
	Data json.RawMessage  `json:"data"`
}

// SignWebhookPayload returns the X-Mobius-Signature header value for body using
// the Mobius outgoing-webhook HMAC-SHA256 format.
func SignWebhookPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return webhookSignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature verifies signatureHeader against body using secret.
// The expected header format is "sha256=<hex>".
func VerifyWebhookSignature(secret string, body []byte, signatureHeader string) error {
	if secret == "" {
		return fmt.Errorf("%w: secret is empty", ErrInvalidWebhookSignature)
	}
	if !strings.HasPrefix(signatureHeader, webhookSignaturePrefix) {
		return fmt.Errorf("%w: missing sha256 prefix", ErrInvalidWebhookSignature)
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signatureHeader, webhookSignaturePrefix))
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrInvalidWebhookSignature)
	}
	expected := SignWebhookPayload(secret, body)
	want, err := hex.DecodeString(strings.TrimPrefix(expected, webhookSignaturePrefix))
	if err != nil {
		return fmt.Errorf("mobius: compute webhook signature: %w", err)
	}
	if !hmac.Equal(got, want) {
		return fmt.Errorf("%w: mismatch", ErrInvalidWebhookSignature)
	}
	return nil
}

// ParseWebhookEvent decodes a Mobius outgoing webhook envelope. The Data field
// remains raw JSON so callers can unmarshal it into the event-specific type
// they care about.
func ParseWebhookEvent(body []byte) (*WebhookEvent, error) {
	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("mobius: parse webhook event: %w", err)
	}
	if event.Type == "" {
		return nil, errors.New("mobius: parse webhook event: missing type")
	}
	return &event, nil
}

// ParseSignedWebhookRequest reads r.Body, verifies the Mobius signature header,
// and decodes the outgoing webhook envelope. The returned body is the exact byte
// slice that was verified, for callers that need to log or reprocess it.
func ParseSignedWebhookRequest(r *http.Request, secret string) (*WebhookEvent, []byte, error) {
	if r == nil {
		return nil, nil, errors.New("mobius: nil webhook request")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("mobius: read webhook request: %w", err)
	}
	if err := VerifyWebhookSignature(secret, body, r.Header.Get(WebhookSignatureHeader)); err != nil {
		return nil, body, err
	}
	event, err := ParseWebhookEvent(body)
	if err != nil {
		return nil, body, err
	}
	return event, body, nil
}
