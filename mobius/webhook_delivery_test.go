package mobius

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSyntheticWebhookPayload(t *testing.T) {
	payload, err := BuildSyntheticWebhookPayload("run.completed", map[string]any{"id": "run_1"})
	require.NoError(t, err)

	assert.JSONEq(t, `{"type":"run.completed","data":{"id":"run_1"}}`, string(payload))
}

func TestDeliverSyntheticWebhook(t *testing.T) {
	var gotBody []byte
	var gotEventType string
	var gotSignature string
	var gotUserAgent string
	var gotVersion string
	var gotTimestamp string
	var gotDeliveryID string
	var gotSecretRef string
	var gotSecretVersion string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEventType = r.Header.Get("X-Mobius-Event-Type")
		gotSignature = r.Header.Get(MobiusSignatureHeader)
		gotUserAgent = r.Header.Get("User-Agent")
		gotVersion = r.Header.Get(MobiusSignatureVersionHeader)
		gotTimestamp = r.Header.Get(MobiusTimestampHeader)
		gotDeliveryID = r.Header.Get(MobiusDeliveryIDHeader)
		gotSecretRef = r.Header.Get(MobiusSecretRefHeader)
		gotSecretVersion = r.Header.Get(MobiusSecretVersionHeader)
		var err error
		gotBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	key := []byte("01234567890123456789012345678901")
	err := DeliverSyntheticWebhook(context.Background(), SyntheticWebhookDelivery{
		URL:           server.URL,
		Key:           key,
		SecretRef:     "mobius/webhook/test",
		SecretVersion: 2,
		DeliveryID:    "delivery_1",
		Timestamp:     1710000000,
		EventType:     "run.completed",
		Data:          map[string]any{"id": "run_1"},
	})
	require.NoError(t, err)

	assert.Equal(t, "run.completed", gotEventType)
	assert.Equal(t, syntheticWebhookUserAgent, gotUserAgent)
	assert.Equal(t, "v1", gotVersion)
	assert.Equal(t, "1710000000", gotTimestamp)
	assert.Equal(t, "delivery_1", gotDeliveryID)
	assert.Equal(t, "mobius/webhook/test", gotSecretRef)
	assert.Equal(t, "2", gotSecretVersion)
	assert.Equal(t, SignDelivery(key, gotBody, "delivery_1", 1710000000), gotSignature)
	assert.JSONEq(t, `{"type":"run.completed","data":{"id":"run_1"}}`, string(gotBody))
}

func TestDeliverSyntheticWebhookReturnsReceiverError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer server.Close()

	err := DeliverSyntheticWebhook(context.Background(), SyntheticWebhookDelivery{
		URL:           server.URL,
		Key:           []byte("01234567890123456789012345678901"),
		SecretRef:     "mobius/webhook/test",
		SecretVersion: 2,
		EventType:     "run.failed",
		Data:          json.RawMessage(`{"id":"run_1"}`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
	assert.Contains(t, err.Error(), "nope")
}
