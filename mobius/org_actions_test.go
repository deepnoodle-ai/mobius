package mobius

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

func orgActionJSON(secret string, versions ...string) string {
	var sb strings.Builder
	sb.WriteString(`{"id":"act_1","name":"crm.sync","endpoint_url":"https://example.com/hook",`)
	sb.WriteString(`"invocation_format":"signed_context_v1","enabled":true,"secret_ref":"osec_abc",`)
	sb.WriteString(`"created_at":"2026-07-17T00:00:00Z","updated_at":"2026-07-17T00:00:00Z",`)
	if secret != "" {
		fmt.Fprintf(&sb, `"signing_secret":%q,`, secret)
	}
	sb.WriteString(`"secret_versions":[`)
	for i, v := range versions {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"version":%d,"status":%q,"created_at":"2026-07-17T00:00:00Z"}`, i+1, v)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

func TestCreateOrganizationActionReturnsDecodedSecretMaterial(t *testing.T) {
	key := []byte("super-secret-signing-key-32bytes")
	encoded := base64.StdEncoding.EncodeToString(key)
	var gotBody map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/organization/actions" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusCreated, orgActionJSON(encoded, "active"))
	}))

	material, err := c.CreateOrganizationAction(context.Background(), api.CreateOrganizationActionRequest{
		Name:        "crm.sync",
		EndpointUrl: "https://example.com/hook",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["name"] != "crm.sync" {
		t.Fatalf("request body = %#v", gotBody)
	}
	if !bytes.Equal(material.KeyBytes, key) {
		t.Fatal("KeyBytes must round-trip the base64 signing_secret")
	}
	if material.SecretRef != "osec_abc" || material.Version != 1 {
		t.Fatalf("material = %+v", material)
	}
	if material.Action.SigningSecret != nil {
		t.Fatal("embedded action must not retain the signing secret")
	}
}

func TestRotateOrganizationActionSecretIdentifiesPendingVersion(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("rotated-key"))
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/organization/actions/act_1/secret/rotate" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, orgActionJSON(encoded, "active", "pending"))
	}))

	material, err := c.RotateOrganizationActionSecret(context.Background(), "act_1")
	if err != nil {
		t.Fatal(err)
	}
	if material.Version != 2 {
		t.Fatalf("rotate must attribute the secret to the newest (pending) version, got %d", material.Version)
	}
	if string(material.KeyBytes) != "rotated-key" {
		t.Fatal("KeyBytes mismatch")
	}
}

func TestOrganizationActionSecretMaterialRejectsInconsistentResponses(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("key"))
	secretValue := "c2VjcmV0LXZhbHVl" // base64("secret-value")

	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing secret",
			body:    orgActionJSON("", "active"),
			wantErr: "missing the one-time signing_secret",
		},
		{
			name:    "no versions",
			body:    orgActionJSON(encoded),
			wantErr: "no secret_versions",
		},
		{
			name:    "wrong newest status",
			body:    orgActionJSON(encoded, "active", "revoked"),
			wantErr: `status "revoked", want "active"`,
		},
		{
			name:    "invalid base64",
			body:    orgActionJSON("not_base64!!", "active"),
			wantErr: "not valid base64",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusCreated, tc.body)
			}))
			_, err := c.CreateOrganizationAction(context.Background(), api.CreateOrganizationActionRequest{
				Name:        "crm.sync",
				EndpointUrl: "https://example.com/hook",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), secretValue) || strings.Contains(err.Error(), encoded) {
				t.Fatalf("error must not leak secret material: %v", err)
			}
		})
	}
}

func TestOrganizationActionAdminRoutes(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/organization/actions":
			if got := r.URL.Query().Get("limit"); got != "10" {
				t.Fatalf("limit = %q", got)
			}
			writeJSON(w, http.StatusOK, `{"items":[`+orgActionJSON("", "active")+`],"has_more":false}`)
		case "GET /v1/organization/actions/act_1":
			writeJSON(w, http.StatusOK, orgActionJSON("", "active"))
		case "PATCH /v1/organization/actions/act_1":
			var req api.UpdateOrganizationActionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil || *req.Enabled {
				t.Fatalf("update body = %#v (%v)", req, err)
			}
			writeJSON(w, http.StatusOK, orgActionJSON("", "active"))
		case "DELETE /v1/organization/actions/act_1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	ctx := context.Background()
	page, err := c.ListOrganizationActions(ctx, &ListOrganizationActionsOptions{Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("list: %v, %#v", err, page)
	}
	if _, err := c.GetOrganizationAction(ctx, "act_1"); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := c.UpdateOrganizationAction(ctx, "act_1", api.UpdateOrganizationActionRequest{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteOrganizationAction(ctx, "act_1"); err != nil {
		t.Fatal(err)
	}
}

func TestActivateOrganizationActionSecretVersionSendsOverlap(t *testing.T) {
	var gotBody map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/organization/actions/act_1/secret/versions/2/activate" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusOK, orgActionJSON("", "retiring", "active"))
	}))

	_, err := c.ActivateOrganizationActionSecretVersion(context.Background(), "act_1", 2, &ActivateOrganizationActionSecretVersionOptions{OverlapSeconds: 0})
	if err != nil {
		t.Fatal(err)
	}
	if overlap, ok := gotBody["overlap_seconds"].(float64); !ok || overlap != 0 {
		t.Fatalf("overlap_seconds = %#v, want explicit 0", gotBody["overlap_seconds"])
	}

	_, err = c.ActivateOrganizationActionSecretVersion(context.Background(), "act_1", 2, &ActivateOrganizationActionSecretVersionOptions{OverlapSeconds: 86401})
	if err == nil || !strings.Contains(err.Error(), "between 0 and 86400") {
		t.Fatalf("err = %v", err)
	}
}

func TestRevokeOrganizationActionSecretVersionSurfacesConflict(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/organization/actions/act_1/secret/versions/1/revoke" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeJSON(w, http.StatusConflict, `{"error":{"code":"secret_version_active","message":"activate another version first"}}`)
	}))

	_, err := c.RevokeOrganizationActionSecretVersion(context.Background(), "act_1", 1)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Code != "secret_version_active" {
		t.Fatalf("err = %v", err)
	}
}
