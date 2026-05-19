package mobius

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

func TestStartDiscussionCreatesChannelInteractionAndOpeningMessage(t *testing.T) {
	type recordedRequest struct {
		method string
		path   string
		body   map[string]any
	}
	var calls []recordedRequest
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			if len(b) > 0 {
				assert.NoError(t, json.Unmarshal(b, &body))
			}
		}
		calls = append(calls, recordedRequest{method: r.Method, path: r.URL.Path, body: body})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/interactions":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, interactionJSON("iact_created", "pending"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/channels":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, channelJSON("chn_1"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/channels/chn_1/messages":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, channelMessageJSON("msg_1", "chn_1"))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/projects/test-project/interactions/"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, interactionJSON(strings.TrimPrefix(r.URL.Path, "/v1/projects/test-project/interactions/"), "completed"))
		default:
			http.NotFound(w, r)
		}
	})
	c, _ := newTestClient(t, h)

	result, err := c.StartDiscussion(context.Background(), StartDiscussionOptions{
		Name:                     "incident-review",
		DisplayName:              "Incident Review",
		AssociatedInteractionIDs: []string{"iact_existing"},
		Interactions: []api.CreateStandaloneInteractionRequest{{
			Kind:    api.InteractionKindReview,
			Message: "Review the incident notes",
			TargetUserIds: &[]string{"usr_1"},
		}},
		OpeningMessage: "Please resolve this together.",
		Wait:           &WaitDiscussionOptions{Timeout: time.Second, PollInterval: time.Millisecond},
	})

	assert.NoError(t, err)
	assert.Equal(t, result.ChannelID, "chn_1")
	assert.Equal(t, result.OpeningMessageID, "msg_1")
	assert.Equal(t, result.InteractionIDs[0], "iact_existing")
	assert.Equal(t, result.InteractionIDs[1], "iact_created")
	assert.Equal(t, len(result.Outcomes), 2)

	// 1 createInteraction + 1 createChannel (with embedded
	// associated_interaction_ids) + 1 sendMessage + 2 GET interaction
	// polls (one per interaction in the Wait loop) = 5 calls.
	assert.Equal(t, len(calls), 5)
	assert.Equal(t, calls[0].path, "/v1/projects/test-project/interactions")
	assert.Equal(t, calls[1].path, "/v1/projects/test-project/channels")
	assert.Equal(t, calls[2].path, "/v1/projects/test-project/channels/chn_1/messages")
	assert.Equal(t, calls[1].body["purpose"], "resolve_interactions")
	assert.Equal(t, calls[1].body["associated_interaction_ids"].([]any)[0], "iact_existing")
	assert.Equal(t, calls[1].body["associated_interaction_ids"].([]any)[1], "iact_created")
	assert.Equal(t, calls[2].body["references"].([]any)[0].(map[string]any)["entity_type"], "interaction")
}

func TestStartDiscussionCancelsCreatedInteractionsWhenSetupFails(t *testing.T) {
	var cancelBody map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/interactions":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, interactionJSON("iact_created", "pending"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/channels":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, channelJSON("chn_1"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/channels/chn_1/messages":
			http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/test-project/interactions/iact_created/cancel":
			b, _ := io.ReadAll(r.Body)
			assert.NoError(t, json.Unmarshal(b, &cancelBody))
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, interactionJSON("iact_created", "cancelled"))
		default:
			http.NotFound(w, r)
		}
	})
	c, _ := newTestClient(t, h)

	_, err := c.StartDiscussion(context.Background(), StartDiscussionOptions{
		Name: "rollback-review",
		Interactions: []api.CreateStandaloneInteractionRequest{{
			Kind:    api.InteractionKindReview,
			Message: "Review the setup",
			TargetUserIds: &[]string{"usr_1"},
		}},
		OpeningMessage: "This will fail.",
	})

	assert.True(t, err != nil)
	assert.Equal(t, cancelBody["reason"], "discussion_start_failed")
}

func channelJSON(id string) string {
	now := "2026-05-17T00:00:00Z"
	return fmt.Sprintf(`{"id":%q,"name":"incident-review","display_name":"Incident Review","kind":"channel","private":true,"created_by":"usr_creator","purpose":"resolve_interactions","completion_behavior":"none","created_at":%q,"updated_at":%q}`, id, now, now)
}

func channelMessageJSON(id, channelID string) string {
	now := "2026-05-17T00:00:00Z"
	return fmt.Sprintf(`{"id":%q,"channel_id":%q,"sender_type":"service_account","sender_id":"svc_1","display":"message","type":"user.message","content":"Please resolve this together.","created_at":%q}`, id, channelID, now)
}

func interactionJSON(id, status string) string {
	now := "2026-05-17T00:00:00Z"
	return fmt.Sprintf(`{"id":%q,"kind":"review","status":%q,"origin":"manual","target_user_ids":["usr_1"],"created_at":%q,"updated_at":%q}`, id, status, now, now)
}
