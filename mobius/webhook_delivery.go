package mobius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const syntheticWebhookUserAgent = "mobius-sdk-webhook-delivery/1"

// SyntheticWebhookDelivery describes a local or test webhook delivery that
// should look like a Mobius-hosted outgoing webhook POST.
type SyntheticWebhookDelivery struct {
	// URL is the receiver endpoint to POST to.
	URL string
	// Key signs the JSON body with the v1 Mobius signed-delivery format.
	Key []byte
	// SecretRef is written to X-Mobius-Secret-Ref.
	SecretRef string
	// SecretVersion is written to X-Mobius-Secret-Version.
	SecretVersion int64
	// DeliveryID is written to X-Mobius-Delivery-Id and Idempotency-Key.
	// When empty, the SDK generates a UUID for local test delivery.
	DeliveryID string
	// Timestamp is written to X-Mobius-Timestamp. When zero, the current unix
	// timestamp is used.
	Timestamp int64
	// EventType is written to the envelope's "type" field and the
	// X-Mobius-Event-Type header.
	EventType string
	// Data is JSON-marshaled into the envelope's "data" field.
	Data any
	// HTTPClient overrides the default 60s SDK HTTP client timeout.
	HTTPClient *http.Client
	// Header is copied onto the request after SDK headers are set. Use this for
	// caller-specific routing headers; Content-Type, X-Mobius-Event-Type, and
	// X-Mobius-Signature are overwritten by the SDK.
	Header http.Header
}

// BuildSyntheticWebhookPayload builds the JSON envelope used by
// DeliverSyntheticWebhook. It is useful when tests need to inspect or persist
// the exact body before delivery.
func BuildSyntheticWebhookPayload(eventType string, data any) ([]byte, error) {
	if eventType == "" {
		return nil, errors.New("mobius: synthetic webhook event type is required")
	}
	payload, err := json.Marshal(map[string]any{
		"type": eventType,
		"data": data,
	})
	if err != nil {
		return nil, fmt.Errorf("mobius: marshal synthetic webhook payload: %w", err)
	}
	return payload, nil
}

// DeliverSyntheticWebhook posts a Mobius-shaped outgoing webhook payload to a
// local or test receiver. It is intended for local development bridges where
// hosted Mobius cannot reach localhost.
func DeliverSyntheticWebhook(ctx context.Context, delivery SyntheticWebhookDelivery) error {
	if delivery.URL == "" {
		return errors.New("mobius: synthetic webhook URL is required")
	}
	if len(delivery.Key) == 0 {
		return errors.New("mobius: synthetic webhook signing key is required")
	}
	if delivery.SecretRef == "" {
		return errors.New("mobius: synthetic webhook secret ref is required")
	}
	if delivery.SecretVersion <= 0 {
		return errors.New("mobius: synthetic webhook secret version is required")
	}
	payload, err := BuildSyntheticWebhookPayload(delivery.EventType, delivery.Data)
	if err != nil {
		return err
	}
	deliveryID := delivery.DeliveryID
	if deliveryID == "" {
		deliveryID = uuid.NewString()
	}
	timestamp := delivery.Timestamp
	if timestamp == 0 {
		timestamp = time.Now().Unix()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.URL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("mobius: build synthetic webhook request: %w", err)
	}
	for key, values := range delivery.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", syntheticWebhookUserAgent)
	req.Header.Set("X-Mobius-Event-Type", delivery.EventType)
	req.Header.Set(MobiusSignatureHeader, SignDelivery(delivery.Key, payload, deliveryID, timestamp))
	req.Header.Set(MobiusSignatureVersionHeader, "v1")
	req.Header.Set(MobiusTimestampHeader, strconv.FormatInt(timestamp, 10))
	req.Header.Set(MobiusDeliveryIDHeader, deliveryID)
	req.Header.Set(MobiusSecretRefHeader, delivery.SecretRef)
	req.Header.Set(MobiusSecretVersionHeader, strconv.FormatInt(delivery.SecretVersion, 10))
	req.Header.Set("Idempotency-Key", deliveryID)

	client := delivery.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mobius: deliver synthetic webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("mobius: synthetic webhook returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
