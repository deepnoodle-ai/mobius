package mobius

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

func TestListActionInvocationsEncodesEveryFilter(t *testing.T) {
	var gotQuery url.Values
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/test-project/action-invocations" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		gotQuery = r.URL.Query()
		writeJSON(w, http.StatusOK, `{"items":[],"has_more":false}`)
	}))

	_, err := c.ListActionInvocations(context.Background(), &ListActionInvocationsOptions{
		RunID:           "run_1",
		JobID:           "job_1",
		EnvironmentID:   "env_1",
		ActionName:      "crm.sync",
		ActionID:        "act_1",
		DefinitionScope: api.ListActionInvocationsParamsDefinitionScopeOrganization,
		SecretVersion:   2,
		DeliveryID:      "dlv_1",
		CorrelationID:   "corr_1",
		Status:          "failed",
		Cursor:          "cur_1",
		Limit:           25,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"run_id":           "run_1",
		"job_id":           "job_1",
		"environment_id":   "env_1",
		"action_name":      "crm.sync",
		"action_id":        "act_1",
		"definition_scope": "organization",
		"secret_version":   "2",
		"delivery_id":      "dlv_1",
		"correlation_id":   "corr_1",
		"status":           "failed",
		"cursor":           "cur_1",
		"limit":            "25",
	}
	if len(gotQuery) != len(want) {
		t.Fatalf("query = %v, want exactly %d params", gotQuery, len(want))
	}
	for k, v := range want {
		if got := gotQuery.Get(k); got != v {
			t.Fatalf("query %s = %q, want %q", k, got, v)
		}
	}
}

func TestListActionInvocationsPreservesProvenanceFields(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Query()) != 0 {
			t.Fatalf("nil options must send no query params, got %v", r.URL.Query())
		}
		writeJSON(w, http.StatusOK, `{
			"items":[{
				"id":"inv_1","action_name":"crm.sync","action_id":"act_1",
				"definition_scope":"organization","secret_version":2,
				"delivery_id":"dlv_1","correlation_id":"corr_1",
				"status":"success","source":"loop","retry_count":0,
				"started_at":"2026-07-17T00:00:00Z","finished_at":"2026-07-17T00:00:01Z"
			}],
			"next_cursor":"cur_2","has_more":true
		}`)
	}))

	page, err := c.ListActionInvocations(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.HasMore {
		t.Fatalf("page = %#v", page)
	}
	entry := page.Items[0]
	if entry.ActionId == nil || *entry.ActionId != "act_1" {
		t.Fatalf("action_id = %v", entry.ActionId)
	}
	if entry.DefinitionScope == nil || *entry.DefinitionScope != api.ActionInvocationEntryDefinitionScopeOrganization {
		t.Fatalf("definition_scope = %v", entry.DefinitionScope)
	}
	if entry.SecretVersion == nil || *entry.SecretVersion != 2 {
		t.Fatalf("secret_version = %v", entry.SecretVersion)
	}
	if entry.DeliveryId == nil || *entry.DeliveryId != "dlv_1" {
		t.Fatalf("delivery_id = %v", entry.DeliveryId)
	}
	if entry.CorrelationId == nil || *entry.CorrelationId != "corr_1" {
		t.Fatalf("correlation_id = %v", entry.CorrelationId)
	}
}
