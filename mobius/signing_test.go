package mobius

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)

	assert.Equal(t, "delivery_1", got.DeliveryID)
	assert.Equal(t, int64(3), got.SecretVersion)
	assert.Equal(t, body, got.Body)
	event, err := ParseWebhookDelivery(got)
	require.NoError(t, err)
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
	require.NoError(t, err)
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
	require.NoError(t, err)
	require.Equal(t, body, got.Body)

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
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidSignedDelivery))
}

func expectSignedDeliveryError(t *testing.T, req *http.Request, key []byte) error {
	t.Helper()
	_, err := VerifySignedDelivery(req, VerifySignedDeliveryOptions{
		Key: key,
		Now: func() time.Time { return time.Unix(1710000005, 0) },
	})
	require.Error(t, err)
	return err
}
