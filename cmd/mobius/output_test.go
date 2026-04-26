package main

import (
	"strings"
	"testing"
)

func TestPickColumnsHonorsPriorityOrder(t *testing.T) {
	items := []any{
		map[string]any{
			"id":         "wf_1",
			"name":       "Workflow",
			"handle":     "wf",
			"created_at": "2025-01-01T00:00:00Z",
			"spec":       map[string]any{"steps": []any{}}, // non-scalar — excluded
			"weird_key":  "x",
		},
	}
	cols := pickColumns(items)
	want := []string{"id", "handle", "name", "created_at", "weird_key"}
	if strings.Join(cols, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", cols, want)
	}
}

func TestFormatCellShortensStringsAndShowsObjectHints(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "—"},
		{"short", "short"},
		{strings.Repeat("a", 80), strings.Repeat("a", 59) + "…"},
		{true, "true"},
		{float64(42), "42"},
		{float64(3.14), "3.14"},
		{[]any{1, 2, 3}, "[3]"},
		{map[string]any{"a": 1, "b": 2}, "{2 fields}"},
	}
	for _, c := range cases {
		got := formatCell(c.in)
		if got != c.want {
			t.Errorf("formatCell(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestListShapeDetection(t *testing.T) {
	listObj := map[string]any{
		"items":       []any{map[string]any{"id": "x"}},
		"has_more":    true,
		"next_cursor": "c1",
	}
	items, more, cursor, ok := listShape(listObj)
	if !ok || len(items) != 1 || !more || cursor != "c1" {
		t.Fatalf("listShape mis-detected: items=%v more=%v cursor=%q ok=%v", items, more, cursor, ok)
	}

	// Single resource (no items) — should not be detected as a list.
	resObj := map[string]any{"id": "x", "name": "y"}
	if _, _, _, ok := listShape(resObj); ok {
		t.Fatalf("single resource mis-detected as list")
	}
}

// TestOrderedKeysPutsPriorityFieldsFirst keeps key/value rendering stable so
// readers can scan resources in a predictable order.
func TestOrderedKeysPutsPriorityFieldsFirst(t *testing.T) {
	obj := map[string]any{
		"zeta":       "z",
		"id":         "i",
		"alpha":      "a",
		"name":       "n",
		"updated_at": "u",
		"handle":     "h",
	}
	got := orderedKeys(obj)
	// Priority order goes: id, handle, name, ..., updated_at, then alphabetical
	// for the rest.
	want := []string{"id", "handle", "name", "updated_at", "alpha", "zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
