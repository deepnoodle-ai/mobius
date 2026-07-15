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

var (
	ErrInvalidSignedDelivery             = errors.New("mobius: invalid signed delivery")
	ErrStaleSignedDelivery               = errors.New("mobius: stale signed delivery")
	ErrUnsupportedActionInvocationSchema = errors.New("mobius: unsupported action invocation schema")
	ErrMalformedActionInvocation         = errors.New("mobius: malformed action invocation")
)

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

type ActionInvocationV1 struct {
	Mobius     ActionInvocationContextV1 `json:"mobius"`
	Parameters map[string]any            `json:"parameters"`
}

type ActionInvocationContextV1 struct {
	SchemaVersion int                      `json:"schema_version"`
	Scope         ActionInvocationScopeV1  `json:"scope"`
	Action        ActionInvocationActionV1 `json:"action"`
	Actor         ActionInvocationActorV1  `json:"actor"`
	Origin        ActionInvocationOriginV1 `json:"origin"`
}

type ActionInvocationScopeV1 struct {
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id"`
}

type ActionInvocationActionV1 struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ActionInvocationActorV1 struct {
	PrincipalID   string `json:"principal_id"`
	PrincipalType string `json:"principal_type"`
	AgentID       string `json:"agent_id,omitempty"`
}

type ActionInvocationOriginV1 struct {
	Kind              string `json:"kind"`
	RunID             string `json:"run_id,omitempty"`
	ChannelExchangeID string `json:"channel_exchange_id,omitempty"`
	LoopID            string `json:"loop_id,omitempty"`
	StepKey           string `json:"step_key,omitempty"`
	AgentTurnID       string `json:"agent_turn_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	ToolCallID        string `json:"tool_call_id,omitempty"`
}

type VerifiedActionInvocationV1 struct {
	DeliveryMeta
	Body       []byte
	Invocation ActionInvocationV1
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
	return VerifySignedDeliveryBytes(body, r.Header, opts)
}

// VerifySignedDeliveryBytes authenticates the exact raw bytes received from
// Mobius. Callers must not parse and reserialize JSON before invoking it.
func VerifySignedDeliveryBytes(body []byte, headers http.Header, opts VerifySignedDeliveryOptions) (*VerifiedDelivery, error) {
	meta, err := ReadDeliveryMeta(headers)
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

// VerifyActionInvocationV1 verifies the signed raw body before parsing its
// identity claims. Unknown JSON fields are ignored for forward compatibility.
func VerifyActionInvocationV1(body []byte, headers http.Header, opts VerifySignedDeliveryOptions) (*VerifiedActionInvocationV1, error) {
	verified, err := VerifySignedDeliveryBytes(body, headers, opts)
	if err != nil {
		return nil, err
	}
	invocation, err := ParseActionInvocationV1(verified)
	if err != nil {
		return nil, err
	}
	return &VerifiedActionInvocationV1{
		DeliveryMeta: verified.DeliveryMeta,
		Body:         verified.Body,
		Invocation:   *invocation,
	}, nil
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

func ParseActionInvocationV1(v *VerifiedDelivery) (*ActionInvocationV1, error) {
	if v == nil {
		return nil, fmt.Errorf("%w: nil verified delivery", ErrMalformedActionInvocation)
	}
	var invocation ActionInvocationV1
	if err := json.Unmarshal(v.Body, &invocation); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON: %v", ErrMalformedActionInvocation, err)
	}
	if err := validateActionInvocationV1(&invocation); err != nil {
		return nil, err
	}
	return &invocation, nil
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
		return fmt.Errorf("%w: %w: timestamp outside max age", ErrInvalidSignedDelivery, ErrStaleSignedDelivery)
	}
	return nil
}

func validateActionInvocationV1(invocation *ActionInvocationV1) error {
	if invocation.Mobius.SchemaVersion == 0 {
		return fmt.Errorf("%w: mobius.schema_version is required", ErrMalformedActionInvocation)
	}
	if invocation.Mobius.SchemaVersion != 1 {
		return fmt.Errorf("%w: %d", ErrUnsupportedActionInvocationSchema, invocation.Mobius.SchemaVersion)
	}
	if strings.TrimSpace(invocation.Mobius.Scope.OrgID) == "" || strings.TrimSpace(invocation.Mobius.Scope.ProjectID) == "" {
		return fmt.Errorf("%w: mobius.scope org_id and project_id are required", ErrMalformedActionInvocation)
	}
	if strings.TrimSpace(invocation.Mobius.Action.ID) == "" || strings.TrimSpace(invocation.Mobius.Action.Name) == "" {
		return fmt.Errorf("%w: mobius.action id and name are required", ErrMalformedActionInvocation)
	}
	actor := invocation.Mobius.Actor
	if strings.TrimSpace(actor.PrincipalID) == "" {
		return fmt.Errorf("%w: mobius.actor.principal_id is required", ErrMalformedActionInvocation)
	}
	switch actor.PrincipalType {
	case "human", "agent", "service", "system":
	default:
		return fmt.Errorf("%w: mobius.actor.principal_type is invalid", ErrMalformedActionInvocation)
	}
	if actor.PrincipalType == "agent" && strings.TrimSpace(actor.AgentID) == "" {
		return fmt.Errorf("%w: mobius.actor.agent_id is required for agent actors", ErrMalformedActionInvocation)
	}
	if actor.PrincipalType != "agent" && strings.TrimSpace(actor.AgentID) != "" {
		return fmt.Errorf("%w: mobius.actor.agent_id is only valid for agent actors", ErrMalformedActionInvocation)
	}
	switch invocation.Mobius.Origin.Kind {
	case "agent_tool_call", "loop_action_step", "direct_action_invoke", "server_internal":
	default:
		return fmt.Errorf("%w: mobius.origin.kind is invalid", ErrMalformedActionInvocation)
	}
	if invocation.Parameters == nil {
		return fmt.Errorf("%w: parameters must be an object", ErrMalformedActionInvocation)
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
