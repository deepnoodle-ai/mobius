package mobius

import "github.com/deepnoodle-ai/mobius/mobius/api"

// ActionResponseContentType opts a signed HTTP action response into the
// envelope that carries output and application context separately.
const ActionResponseContentType = "application/vnd.mobius.action+json"

// RuntimeContextItem is one named application-state snapshot. It is shared by
// agent invocation requests and signed HTTP action response envelopes.
type RuntimeContextItem = api.RuntimeContextItem

// ActionResponseEnvelope is the canonical response body for a customer-owned
// signed HTTP action. Set Content-Type to ActionResponseContentType when
// returning it; application/json retains the legacy whole-body result contract.
type ActionResponseEnvelope struct {
	Output  any                  `json:"output,omitempty"`
	Context []RuntimeContextItem `json:"context,omitempty"`
}
