package mobius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// writeJSON writes a JSON response body with the content type the generated
// client requires to populate its typed response fields.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestListAgentMemoryEntriesEncodesSearchParams(t *testing.T) {
	var gotQuery url.Values
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/test-project/agents/agent_1/memory/entries" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		gotQuery = r.URL.Query()
		writeJSON(w, http.StatusOK, `{
			"items":[{"key":"prefs","kind":"fact","entry_id":"mem_1","importance":50,"pinned":false,"version":3,"created_at":"2026-07-01T00:00:00Z","updated_at":"2026-07-02T00:00:00Z"}],
			"has_more":false,
			"search_coverage":{"indexed_entries":9,"total_entries":10,"complete":false}
		}`)
	}))

	page, err := c.ListAgentMemoryEntries(context.Background(), "agent_1", &ListAgentMemoryEntriesOptions{
		Query:      "preferences",
		SearchMode: api.MemorySearchMode("hybrid"),
		Kind:       api.MemoryKind("fact"),
		Cursor:     "cur_1",
		Limit:      25,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"query":       "preferences",
		"search_mode": "hybrid",
		"kind":        "fact",
		"cursor":      "cur_1",
		"limit":       "25",
	}
	for k, v := range want {
		if got := gotQuery.Get(k); got != v {
			t.Fatalf("query %s = %q, want %q", k, got, v)
		}
	}
	if len(page.Items) != 1 || page.Items[0].Key != "prefs" {
		t.Fatalf("items = %#v", page.Items)
	}
	if page.SearchCoverage == nil || page.SearchCoverage.Complete || page.SearchCoverage.IndexedEntries != 9 {
		t.Fatalf("search coverage must be preserved, got %#v", page.SearchCoverage)
	}
}

func TestListAgentMemoryEntriesSurfacesSemanticUnavailableWithoutDowngrade(t *testing.T) {
	requests := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("search_mode"); got != "semantic" {
			t.Fatalf("SDK downgraded search_mode to %q", got)
		}
		writeJSON(w, http.StatusServiceUnavailable, `{"error":{"code":"memory_semantic_search_unavailable","message":"index offline"}}`)
	}))

	_, err := c.ListAgentMemoryEntries(context.Background(), "agent_1", &ListAgentMemoryEntriesOptions{
		Query:      "preferences",
		SearchMode: api.MemorySearchMode("semantic"),
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != "memory_semantic_search_unavailable" {
		t.Fatalf("apiErr = %#v", apiErr)
	}
	if requests == 0 {
		t.Fatal("no request reached the server")
	}
}

func TestGetSaveDeleteAgentMemoryRoutes(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/projects/test-project/agents/agent_1/memory":
			writeJSON(w, http.StatusOK, `{"agent_id":"agent_1","entry_count":2,"counts_by_kind":{"fact":2},"updated_at":"2026-07-17T00:00:00Z"}`)
		case "PUT /v1/projects/test-project/agents/agent_1/memory/entries/prefs":
			var req api.SaveAgentMemoryEntryRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content != "dark mode" {
				t.Fatalf("save body = %#v (%v)", req, err)
			}
			writeJSON(w, http.StatusCreated, `{"key":"prefs","kind":"fact","entry_id":"mem_1","importance":50,"pinned":false,"version":1,"created_at":"2026-07-17T00:00:00Z","updated_at":"2026-07-17T00:00:00Z"}`)
		case "DELETE /v1/projects/test-project/agents/agent_1/memory/entries/prefs":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	memory, err := c.GetAgentMemory(context.Background(), "agent_1")
	if err != nil {
		t.Fatal(err)
	}
	if memory.EntryCount != 2 || memory.CountsByKind["fact"] != 2 {
		t.Fatalf("memory = %#v", memory)
	}

	entry, err := c.SaveAgentMemoryEntry(context.Background(), "agent_1", "prefs", api.SaveAgentMemoryEntryRequest{Content: "dark mode"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.EntryId != "mem_1" || entry.Version != 1 {
		t.Fatalf("entry = %#v", entry)
	}

	if err := c.DeleteAgentMemoryEntry(context.Background(), "agent_1", "prefs"); err != nil {
		t.Fatal(err)
	}
}

func TestSyncAgentMemoryDrainsChangePages(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/test-project/agents/agent_1/memory/changes" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		switch r.URL.Query().Get("after") {
		case "cur_0":
			writeJSON(w, http.StatusOK, `{"items":[`+memoryChangeJSON("chg_1", 1)+`],"has_more":true,"next_cursor":"cur_1"}`)
		case "cur_1":
			writeJSON(w, http.StatusOK, `{"items":[`+memoryChangeJSON("chg_2", 2)+`],"has_more":false,"next_cursor":"cur_2"}`)
		default:
			t.Fatalf("unexpected after = %q", r.URL.Query().Get("after"))
		}
	}))

	result, err := c.SyncAgentMemory(context.Background(), "agent_1", "cur_0")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reset {
		t.Fatal("unexpected reset")
	}
	if len(result.Changes) != 2 || result.Changes[0].Id != "chg_1" || result.Changes[1].Id != "chg_2" {
		t.Fatalf("changes = %#v", result.Changes)
	}
	if result.NextCursor != "cur_2" {
		t.Fatalf("next cursor = %q", result.NextCursor)
	}
	if len(result.Entries) != 0 {
		t.Fatalf("entries should be empty without a reset: %#v", result.Entries)
	}
}

func TestSyncAgentMemoryRecoversFromExpiredCursor(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/test-project/agents/agent_1/memory/changes":
			if after := r.URL.Query().Get("after"); after == "cur_stale" {
				writeJSON(w, http.StatusGone, `{"error":{"code":"memory_cursor_expired","message":"cursor predates retained history"}}`)
				return
			}
			// Fresh traversal: retained history fits in one page.
			writeJSON(w, http.StatusOK, `{"items":[`+memoryChangeJSON("chg_9", 9)+`],"has_more":false,"next_cursor":"cur_fresh"}`)
		case "/v1/projects/test-project/agents/agent_1/memory/entries":
			switch r.URL.Query().Get("cursor") {
			case "":
				writeJSON(w, http.StatusOK, `{"items":[`+memoryEntryJSON("mem_1", "prefs")+`],"has_more":true,"next_cursor":"ecur_1"}`)
			case "ecur_1":
				writeJSON(w, http.StatusOK, `{"items":[`+memoryEntryJSON("mem_2", "style")+`],"has_more":false}`)
			default:
				t.Fatalf("unexpected entries cursor %q", r.URL.Query().Get("cursor"))
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))

	result, err := c.SyncAgentMemory(context.Background(), "agent_1", "cur_stale")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset {
		t.Fatal("expected reset after 410")
	}
	if len(result.Changes) != 0 {
		t.Fatalf("reset result should carry no changes: %#v", result.Changes)
	}
	if len(result.Entries) != 2 || result.Entries[0].Key != "prefs" || result.Entries[1].Key != "style" {
		t.Fatalf("entries = %#v", result.Entries)
	}
	if result.NextCursor != "cur_fresh" {
		t.Fatalf("next cursor = %q", result.NextCursor)
	}
}

func TestSyncAgentMemoryPropagatesNonCursorErrors(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"no such agent"}}`)
	}))

	_, err := c.SyncAgentMemory(context.Background(), "agent_missing", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v", err)
	}
}

func memoryChangeJSON(id string, version int) string {
	return fmt.Sprintf(`{"id":%q,"agent_id":"agent_1","memory_entry_id":"mem_1","memory_key":"prefs","operation":"updated","version":%d,"reason":"remembered","created_at":"2026-07-17T00:00:00Z"}`, id, version)
}

func memoryEntryJSON(id, key string) string {
	return fmt.Sprintf(`{"key":%q,"kind":"fact","entry_id":%q,"importance":50,"pinned":false,"version":1,"created_at":"2026-07-17T00:00:00Z","updated_at":"2026-07-17T00:00:00Z"}`, key, id)
}
