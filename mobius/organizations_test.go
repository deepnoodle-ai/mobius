package mobius

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
)

func TestOAuthReturnOriginsRoutes(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/organization/oauth-return-origins":
			writeJSON(w, http.StatusOK, `{"origins":["https://app.partner.example"]}`)
		case "PUT /v1/organization/oauth-return-origins":
			var body struct {
				Origins []string `json:"origins"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, []string{"https://app.partner.example"}, body.Origins)
			writeJSON(w, http.StatusOK, `{"origins":["https://app.partner.example"]}`)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	ctx := context.Background()
	got, err := c.GetOAuthReturnOrigins(ctx)
	assert.NoError(t, err)
	assert.Equal(t, []string{"https://app.partner.example"}, got.Origins)

	replaced, err := c.ReplaceOAuthReturnOrigins(ctx, []string{"https://app.partner.example"})
	assert.NoError(t, err)
	assert.Equal(t, []string{"https://app.partner.example"}, replaced.Origins)
}

func TestReplaceOAuthReturnOriginsEmptyListDisablesEmbeddedReturn(t *testing.T) {
	var raw json.RawMessage
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&raw))
		writeJSON(w, http.StatusOK, `{"origins":[]}`)
	}))

	got, err := c.ReplaceOAuthReturnOrigins(context.Background(), nil)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(got.Origins))
	// A nil slice must serialize as [], not null — an empty list is the
	// documented way to disable embedded return.
	assert.Equal(t, `{"origins":[]}`, string(raw))
}
