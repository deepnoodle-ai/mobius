package mobius

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// ListAgentMemoryEntriesOptions filters, searches, and paginates an agent's
// memory entries.
type ListAgentMemoryEntriesOptions struct {
	// Query optionally searches entry keys, kinds, summaries, and content.
	// Omit to list.
	Query string
	// SearchMode ranks a non-blank Query: keyword (the server default),
	// semantic, or hybrid. Semantic and hybrid surface a 503
	// memory_semantic_search_unavailable [APIError] when the index is
	// unavailable; the SDK never downgrades to keyword silently because that
	// would change result semantics — retry or fall back explicitly.
	SearchMode api.MemorySearchMode
	// Kind filters to a single memory kind.
	Kind   api.MemoryKind
	Cursor string
	Limit  int
}

// ListAgentMemoryChangesOptions paginates the append-only memory change feed.
type ListAgentMemoryChangesOptions struct {
	// After is the opaque cursor returned as next_cursor by the previous
	// page. Omit on the first request to read retained changes oldest-first.
	After string
	Limit int
}

// MemorySyncResult is one bounded synchronization step of an agent's memory
// change feed, returned by [Client.SyncAgentMemory].
type MemorySyncResult struct {
	// Reset reports that the supplied cursor predated retained history
	// (HTTP 410). Entries then carries a full current snapshot to replace
	// local state, and Changes is empty.
	Reset bool
	// Changes are all feed items after the supplied cursor, drained across
	// pages. Empty when Reset is true.
	Changes []api.AgentMemoryChange
	// Entries is the full current entry snapshot, present only when Reset is
	// true.
	Entries []api.AgentMemoryEntry
	// NextCursor is the new feed position to persist for the next call.
	NextCursor string
}

// GetAgentMemory returns a summary of an agent's private memory.
func (c *Client) GetAgentMemory(ctx context.Context, agentID string) (*api.AgentMemory, error) {
	resp, err := c.ac.GetAgentMemoryWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID))
	if err != nil {
		return nil, fmt.Errorf("mobius: get agent memory: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("get agent memory", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// ListAgentMemoryEntries lists or searches an agent's memory entries. The
// response preserves search_coverage so callers can see when semantic or
// hybrid results ranked only a partially indexed subset.
func (c *Client) ListAgentMemoryEntries(ctx context.Context, agentID string, opts *ListAgentMemoryEntriesOptions) (*api.AgentMemoryEntryListResponse, error) {
	params := &api.ListAgentMemoryEntriesParams{}
	if opts != nil {
		if opts.Query != "" {
			params.Query = &opts.Query
		}
		if opts.SearchMode != "" {
			params.SearchMode = &opts.SearchMode
		}
		if opts.Kind != "" {
			params.Kind = &opts.Kind
		}
		if opts.Cursor != "" {
			params.Cursor = &opts.Cursor
		}
		if opts.Limit > 0 {
			params.Limit = &opts.Limit
		}
	}
	resp, err := c.ac.ListAgentMemoryEntriesWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list agent memory entries: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("list agent memory entries", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// SaveAgentMemoryEntry creates or updates the memory entry stored under key.
func (c *Client) SaveAgentMemoryEntry(ctx context.Context, agentID, key string, req api.SaveAgentMemoryEntryRequest) (*api.AgentMemoryEntry, error) {
	resp, err := c.ac.SaveAgentMemoryEntryWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID), api.MemoryKeyParam(key), req)
	if err != nil {
		return nil, fmt.Errorf("mobius: save agent memory entry: %w", err)
	}
	if resp.JSON200 != nil {
		return resp.JSON200, nil
	}
	if resp.JSON201 != nil {
		return resp.JSON201, nil
	}
	return nil, unexpectedProjectResourceStatus("save agent memory entry", resp.HTTPResponse, resp.Body)
}

// DeleteAgentMemoryEntry deletes the memory entry stored under key.
func (c *Client) DeleteAgentMemoryEntry(ctx context.Context, agentID, key string) error {
	resp, err := c.ac.DeleteAgentMemoryEntryWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID), api.MemoryKeyParam(key))
	if err != nil {
		return fmt.Errorf("mobius: delete agent memory entry: %w", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return unexpectedProjectResourceStatus("delete agent memory entry", resp.HTTPResponse, resp.Body)
	}
	return nil
}

// ListAgentMemoryChanges returns one page of the content-free, append-only
// memory change feed. A 410 [APIError] means the cursor predates retained
// history; recover with [Client.SyncAgentMemory] or by relisting entries.
func (c *Client) ListAgentMemoryChanges(ctx context.Context, agentID string, opts *ListAgentMemoryChangesOptions) (*api.AgentMemoryChangeListResponse, error) {
	params := &api.ListAgentMemoryChangesParams{}
	if opts != nil {
		if opts.After != "" {
			params.After = &opts.After
		}
		if opts.Limit > 0 {
			params.Limit = &opts.Limit
		}
	}
	resp, err := c.ac.ListAgentMemoryChangesWithResponse(ctx, api.ProjectHandleParam(c.projectHandle), api.IDParam(agentID), params)
	if err != nil {
		return nil, fmt.Errorf("mobius: list agent memory changes: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, unexpectedProjectResourceStatus("list agent memory changes", resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// SyncAgentMemory advances a memory change-feed consumer by one bounded
// synchronization step: it drains every change page after cursor and returns
// the new feed position to persist. When the cursor has expired (HTTP 410) it
// recovers explicitly — establishing a fresh feed position and returning a
// full entry snapshot with Reset set — instead of failing or silently
// replaying. Pass an empty cursor on first use. Polling cadence and retry
// policy stay with the caller; this makes no timing decisions.
func (c *Client) SyncAgentMemory(ctx context.Context, agentID, cursor string) (*MemorySyncResult, error) {
	changes, next, err := c.drainAgentMemoryChanges(ctx, agentID, cursor)
	if err == nil {
		return &MemorySyncResult{Changes: changes, NextCursor: next}, nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusGone {
		return nil, err
	}
	// The cursor predates retained history. Take the fresh feed position
	// BEFORE the snapshot: a mutation racing the snapshot then replays after
	// the new cursor (versions make replays detectable) instead of being lost.
	_, next, err = c.drainAgentMemoryChanges(ctx, agentID, "")
	if err != nil {
		return nil, err
	}
	entries, err := c.drainAgentMemoryEntries(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return &MemorySyncResult{Reset: true, Entries: entries, NextCursor: next}, nil
}

func (c *Client) drainAgentMemoryChanges(ctx context.Context, agentID, after string) ([]api.AgentMemoryChange, string, error) {
	var changes []api.AgentMemoryChange
	cursor := after
	for {
		page, err := c.ListAgentMemoryChanges(ctx, agentID, &ListAgentMemoryChangesOptions{After: cursor})
		if err != nil {
			return nil, "", err
		}
		changes = append(changes, page.Items...)
		cursor = page.NextCursor
		if !page.HasMore {
			return changes, cursor, nil
		}
	}
}

func (c *Client) drainAgentMemoryEntries(ctx context.Context, agentID string) ([]api.AgentMemoryEntry, error) {
	var entries []api.AgentMemoryEntry
	cursor := ""
	for {
		page, err := c.ListAgentMemoryEntries(ctx, agentID, &ListAgentMemoryEntriesOptions{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		entries = append(entries, page.Items...)
		if !page.HasMore || page.NextCursor == nil || *page.NextCursor == "" {
			return entries, nil
		}
		cursor = *page.NextCursor
	}
}
