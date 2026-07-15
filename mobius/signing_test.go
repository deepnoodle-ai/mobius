package mobius

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/assert"
)

func signedDeliveryRequest(body []byte, key []byte, deliveryID string, timestamp int64) *http.Request {
	req := httptest.NewRequest("POST", "/deliveries", bytes.NewReader(body))
	req.Header.Set(MobiusSignatureVersionHeader, "v1")
	req.Header.Set(MobiusSignatureHeader, SignDelivery(key, body, deliveryID, timestamp))
	req.Header.Set(MobiusTimestampHeader, strconv.FormatInt(timestamp, 10))
	req.Header.Set(MobiusDeliveryIDHeader, deliveryID)
	req.Header.Set(MobiusSecretRefHeader, "mobius/webhook/wbh_1")
	req.Header.Set(MobiusSecretVersionHeader, "3")
	return req
}

func TestVerifySignedDeliveryWithKey(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	body := []byte(`{"type":"run.completed","data":{"id":"run_1"}}`)
	req := signedDeliveryRequest(body, key, "delivery_1", 1710000000)

	got, err := VerifySignedDelivery(req, VerifySignedDeliveryOptions{
		Key: key,
		Now: func() time.Time { return time.Unix(1710000005, 0) },
	})
	assert.NoError(t, err)

	assert.Equal(t, "delivery_1", got.DeliveryID)
	assert.Equal(t, int64(3), got.SecretVersion)
	assert.Equal(t, body, got.Body)
	event, err := ParseWebhookDelivery(got)
	assert.NoError(t, err)
	assert.Equal(t, WebhookEventRunCompleted, event.Type)
}

func TestVerifySignedDeliveryWithResolver(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	body := []byte(`{"ok":true}`)
	req := signedDeliveryRequest(body, key, "delivery_2", 1710000000)

	got, err := VerifySignedDelivery(req, VerifySignedDeliveryOptions{
		ResolveKey: func(meta DeliveryMeta) ([]byte, error) {
			assert.Equal(t, "mobius/webhook/wbh_1", meta.SecretRef)
			assert.Equal(t, int64(3), meta.SecretVersion)
			return key, nil
		},
		Now: func() time.Time { return time.Unix(1710000005, 0) },
	})
	assert.NoError(t, err)
	assert.Equal(t, "delivery_2", got.DeliveryID)
}

func TestVerifySignedDeliveryRejectsTampering(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	body := []byte(`{"ok":true}`)
	req := signedDeliveryRequest(body, key, "delivery_3", 1710000000)

	got, err := VerifySignedDelivery(req, VerifySignedDeliveryOptions{
		Key: key,
		Now: func() time.Time { return time.Unix(1710000005, 0) },
	})
	assert.NoError(t, err)
	assert.Equal(t, body, got.Body)

	req = signedDeliveryRequest([]byte(`{"ok":false}`), key, "delivery_3", 1710000000)
	req.Header.Set(MobiusSignatureHeader, SignDelivery(key, body, "delivery_3", 1710000000))
	err = expectSignedDeliveryError(t, req, key)
	assert.True(t, errors.Is(err, ErrInvalidSignedDelivery))
}

func TestVerifySignedDeliveryRejectsStaleTimestamp(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	req := signedDeliveryRequest([]byte(`{"ok":true}`), key, "delivery_4", 1710000000)

	_, err := VerifySignedDelivery(req, VerifySignedDeliveryOptions{
		Key: key,
		Now: func() time.Time { return time.Unix(1710000601, 0) },
	})
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidSignedDelivery))
	assert.True(t, errors.Is(err, ErrStaleSignedDelivery))
}

func TestVerifyActionInvocationV1GoldenFixture(t *testing.T) {
	body, err := os.ReadFile("../internal/testdata/action-invocation-v1.json")
	assert.NoError(t, err)
	key := []byte("01234567890123456789012345678901")
	assert.Equal(t, "sha256=9db53d763fc7bf16d9df33322860b5f1f6fbf77c4c21cdce26c13570d97a61e7", SignDelivery(key, body, "delivery_fixture_1", 1710000000))
	req := signedDeliveryRequest(body, key, "delivery_fixture_1", 1710000000)

	got, err := VerifyActionInvocationV1(body, req.Header, VerifySignedDeliveryOptions{
		Key: key,
		Now: func() time.Time { return time.Unix(1710000005, 0) },
	})
	assert.NoError(t, err)
	assert.Equal(t, "org_fixture", got.Invocation.Mobius.Scope.OrgID)
	assert.Equal(t, "prj_fixture", got.Invocation.Mobius.Scope.ProjectID)
	assert.Equal(t, "act_fixture", got.Invocation.Mobius.Action.ID)
	assert.Equal(t, "agt_fixture", got.Invocation.Mobius.Actor.AgentID)
	assert.Equal(t, "agent_tool_call", got.Invocation.Mobius.Origin.Kind)
	assert.Equal(t, "doc_fixture", got.Invocation.Parameters["document_id"])
}

func TestParseActionInvocationV1RejectsMalformedAndUnsupportedEnvelopes(t *testing.T) {
	tests := []struct {
		name string
		body string
		err  error
	}{
		{name: "missing actor", body: `{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"origin":{"kind":"direct_action_invoke"}},"parameters":{}}`, err: ErrMalformedActionInvocation},
		{name: "agent missing agent id", body: `{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"agent"},"origin":{"kind":"agent_tool_call"}},"parameters":{}}`, err: ErrMalformedActionInvocation},
		{name: "unknown schema", body: `{"mobius":{"schema_version":2},"parameters":{}}`, err: ErrUnsupportedActionInvocationSchema},
		{name: "parameters is not object", body: `{"mobius":{"schema_version":1,"scope":{"org_id":"org_1","project_id":"prj_1"},"action":{"id":"act_1","name":"test.action"},"actor":{"principal_id":"prn_1","principal_type":"human"},"origin":{"kind":"direct_action_invoke"}},"parameters":[]}`, err: ErrMalformedActionInvocation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseActionInvocationV1(&VerifiedDelivery{Body: []byte(tt.body)})
			assert.Error(t, err)
			assert.True(t, errors.Is(err, tt.err))
		})
	}
}

func expectSignedDeliveryError(t *testing.T, req *http.Request, key []byte) error {
	t.Helper()
	_, err := VerifySignedDelivery(req, VerifySignedDeliveryOptions{
		Key: key,
		Now: func() time.Time { return time.Unix(1710000005, 0) },
	})
	assert.Error(t, err)
	return err
}
