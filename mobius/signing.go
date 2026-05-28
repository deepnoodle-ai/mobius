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
	"strconv"
	"strings"
	"time"
)

const (
	MobiusSignatureHeader        = "X-Mobius-Signature"
	MobiusSignatureVersionHeader = "X-Mobius-Signature-Version"
	MobiusTimestampHeader        = "X-Mobius-Timestamp"
	MobiusDeliveryIDHeader       = "X-Mobius-Delivery-Id"
	MobiusSecretRefHeader        = "X-Mobius-Secret-Ref"
	MobiusSecretVersionHeader    = "X-Mobius-Secret-Version"

	signedDeliveryVersion = "v1"
	signedDeliveryPrefix  = "sha256="
	defaultMaxAge         = 5 * time.Minute
)

var ErrInvalidSignedDelivery = errors.New("mobius: invalid signed delivery")

type DeliveryMeta struct {
	SignatureVersion string
	Signature        string
	Timestamp        int64
	DeliveryID       string
	SecretRef        string
	SecretVersion    int64
}

type VerifiedDelivery struct {
	DeliveryMeta
	Body []byte
}

type SigningKeyResolver func(meta DeliveryMeta) ([]byte, error)

type VerifySignedDeliveryOptions struct {
	Key        []byte
	ResolveKey SigningKeyResolver
	MaxAge     time.Duration
	Now        func() time.Time
}

func ReadDeliveryMeta(h http.Header) (DeliveryMeta, error) {
	meta := DeliveryMeta{
		SignatureVersion: h.Get(MobiusSignatureVersionHeader),
		Signature:        h.Get(MobiusSignatureHeader),
		DeliveryID:       h.Get(MobiusDeliveryIDHeader),
		SecretRef:        h.Get(MobiusSecretRefHeader),
	}
	if meta.SignatureVersion != signedDeliveryVersion {
		return DeliveryMeta{}, fmt.Errorf("%w: unsupported signature version", ErrInvalidSignedDelivery)
	}
	if meta.Signature == "" {
		return DeliveryMeta{}, fmt.Errorf("%w: missing signature", ErrInvalidSignedDelivery)
	}
	if meta.DeliveryID == "" {
		return DeliveryMeta{}, fmt.Errorf("%w: missing delivery id", ErrInvalidSignedDelivery)
	}
	if meta.SecretRef == "" {
		return DeliveryMeta{}, fmt.Errorf("%w: missing secret ref", ErrInvalidSignedDelivery)
	}
	timestamp, err := strconv.ParseInt(h.Get(MobiusTimestampHeader), 10, 64)
	if err != nil || timestamp <= 0 {
		return DeliveryMeta{}, fmt.Errorf("%w: invalid timestamp", ErrInvalidSignedDelivery)
	}
	meta.Timestamp = timestamp
	version, err := strconv.ParseInt(h.Get(MobiusSecretVersionHeader), 10, 64)
	if err != nil || version <= 0 {
		return DeliveryMeta{}, fmt.Errorf("%w: invalid secret version", ErrInvalidSignedDelivery)
	}
	meta.SecretVersion = version
	return meta, nil
}

func SignDelivery(key, body []byte, deliveryID string, timestamp int64) string {
	mac := hmac.New(sha256.New, key)
	writeSignedDeliveryCanonical(mac, body, deliveryID, timestamp)
	return signedDeliveryPrefix + hex.EncodeToString(mac.Sum(nil))
}

func VerifySignedDelivery(r *http.Request, opts VerifySignedDeliveryOptions) (*VerifiedDelivery, error) {
	if r == nil {
		return nil, errors.New("mobius: nil signed delivery request")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("mobius: read signed delivery: %w", err)
	}
	meta, err := ReadDeliveryMeta(r.Header)
	if err != nil {
		return nil, err
	}
	if err := verifyDeliveryFreshness(meta.Timestamp, opts); err != nil {
		return nil, err
	}
	key := opts.Key
	if len(key) == 0 && opts.ResolveKey != nil {
		key, err = opts.ResolveKey(meta)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve key: %v", ErrInvalidSignedDelivery, err)
		}
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: signing key is required", ErrInvalidSignedDelivery)
	}
	if err := verifyDeliverySignature(key, body, meta); err != nil {
		return nil, err
	}
	return &VerifiedDelivery{DeliveryMeta: meta, Body: body}, nil
}

func ParseWebhookDelivery(v *VerifiedDelivery) (*WebhookEvent, error) {
	if v == nil {
		return nil, errors.New("mobius: nil verified delivery")
	}
	return parseWebhookEvent(v.Body)
}

func ParseActionInvocation(v *VerifiedDelivery) (map[string]any, error) {
	return parseVerifiedDeliveryJSON(v)
}

func ParseInteractionCallback(v *VerifiedDelivery) (map[string]any, error) {
	return parseVerifiedDeliveryJSON(v)
}

func verifyDeliveryFreshness(timestamp int64, opts VerifySignedDeliveryOptions) error {
	maxAge := opts.MaxAge
	if maxAge == 0 {
		maxAge = defaultMaxAge
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	age := now().Sub(time.Unix(timestamp, 0))
	if age < 0 {
		age = -age
	}
	if age > maxAge {
		return fmt.Errorf("%w: timestamp outside max age", ErrInvalidSignedDelivery)
	}
	return nil
}

func verifyDeliverySignature(key, body []byte, meta DeliveryMeta) error {
	if !strings.HasPrefix(meta.Signature, signedDeliveryPrefix) {
		return fmt.Errorf("%w: missing sha256 prefix", ErrInvalidSignedDelivery)
	}
	got, err := hex.DecodeString(strings.TrimPrefix(meta.Signature, signedDeliveryPrefix))
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrInvalidSignedDelivery)
	}
	expected := SignDelivery(key, body, meta.DeliveryID, meta.Timestamp)
	want, err := hex.DecodeString(strings.TrimPrefix(expected, signedDeliveryPrefix))
	if err != nil {
		return fmt.Errorf("mobius: compute signed delivery signature: %w", err)
	}
	if !hmac.Equal(got, want) {
		return fmt.Errorf("%w: mismatch", ErrInvalidSignedDelivery)
	}
	return nil
}

func parseVerifiedDeliveryJSON(v *VerifiedDelivery) (map[string]any, error) {
	if v == nil {
		return nil, errors.New("mobius: nil verified delivery")
	}
	var out map[string]any
	if err := json.Unmarshal(v.Body, &out); err != nil {
		return nil, fmt.Errorf("mobius: parse signed delivery: %w", err)
	}
	return out, nil
}

func writeSignedDeliveryCanonical(w io.Writer, body []byte, deliveryID string, timestamp int64) {
	_, _ = io.WriteString(w, signedDeliveryVersion)
	_, _ = io.WriteString(w, ".")
	_, _ = io.WriteString(w, deliveryID)
	_, _ = io.WriteString(w, ".")
	_, _ = io.WriteString(w, strconv.FormatInt(timestamp, 10))
	_, _ = io.WriteString(w, ".")
	_, _ = w.Write(body)
}
