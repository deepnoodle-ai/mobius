package mobius

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEventType = r.Header.Get("X-Mobius-Event-Type")
		gotSignature = r.Header.Get("X-Mobius-Signature")
		gotUserAgent = r.Header.Get("User-Agent")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := DeliverSyntheticWebhook(context.Background(), SyntheticWebhookDelivery{
		URL:       server.URL,
		Secret:    "secret",
		EventType: "run.completed",
		Data:      map[string]any{"id": "run_1"},
	})
	require.NoError(t, err)

	assert.Equal(t, "run.completed", gotEventType)
	assert.Equal(t, syntheticWebhookUserAgent, gotUserAgent)
	assert.Equal(t, expectedSyntheticSignature("secret", gotBody), gotSignature)
	assert.JSONEq(t, `{"type":"run.completed","data":{"id":"run_1"}}`, string(gotBody))
}

func TestDeliverSyntheticWebhookReturnsReceiverError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer server.Close()

	err := DeliverSyntheticWebhook(context.Background(), SyntheticWebhookDelivery{
		URL:       server.URL,
		Secret:    "secret",
		EventType: "run.failed",
		Data:      json.RawMessage(`{"id":"run_1"}`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
	assert.Contains(t, err.Error(), "nope")
}

func expectedSyntheticSignature(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
