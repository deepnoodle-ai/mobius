package mobius

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyWebhookSignature(t *testing.T) {
	body := []byte(`{"type":"run.completed","data":{"id":"run_1"}}`)
	sig := SignWebhookPayload("secret", body)

	require.NoError(t, VerifyWebhookSignature("secret", body, sig))

	err := VerifyWebhookSignature("secret", body, sig[:len(sig)-1]+"0")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidWebhookSignature))
}

func TestVerifyWebhookSignatureRejectsMalformedHeader(t *testing.T) {
	body := []byte(`{"type":"run.completed","data":{}}`)

	for _, sig := range []string{"", "bad", "sha256=not-hex"} {
		err := VerifyWebhookSignature("secret", body, sig)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidWebhookSignature))
	}
}

func TestParseWebhookEvent(t *testing.T) {
	event, err := ParseWebhookEvent([]byte(`{"type":"run.failed","data":{"id":"run_1"}}`))
	require.NoError(t, err)

	assert.Equal(t, WebhookEventRunFailed, event.Type)
	assert.JSONEq(t, `{"id":"run_1"}`, string(event.Data))
}

func TestParseSignedWebhookRequest(t *testing.T) {
	body := []byte(`{"type":"run.completed","data":{"id":"run_1"}}`)
	req := httptest.NewRequest("POST", "/webhooks/mobius", bytes.NewReader(body))
	req.Header.Set(WebhookSignatureHeader, SignWebhookPayload("secret", body))

	event, gotBody, err := ParseSignedWebhookRequest(req, "secret")
	require.NoError(t, err)

	assert.Equal(t, WebhookEventRunCompleted, event.Type)
	assert.Equal(t, body, gotBody)
}
