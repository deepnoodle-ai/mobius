package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

// stdoutIsTerminal reports whether ctx.Stdout() is an interactive terminal.
// Falls back to checking the process os.Stdout when the writer is not a file.
func stdoutIsTerminal(ctx *cli.Context) bool {
	if f, ok := ctx.Stdout().(*os.File); ok {
		return term.IsTerminal(int(f.Fd()))
	}
	// Test harnesses use a bytes.Buffer; treat as non-TTY so output stays
	// machine-parseable.
	return false
}

// renderPretty renders a JSON response body as a TTY-friendly view: a table
// for list responses, a key/value stack for single resources. Falls back to
// pretty-printed JSON when the body isn't JSON or has a shape we can't
// confidently render. Writes to ctx.Stdout().
//
// fields is optional — when non-nil it overrides the auto-picked column
// set (lists) or scalar filter (single resources). Used to honor a
// user-supplied --fields projection in pretty mode.
func renderPretty(ctx *cli.Context, body []byte, fields ...string) error {
	if len(body) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		fmt.Fprintln(ctx.Stdout(), string(body))
		return nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return printJSONFallback(ctx, body)
	}
	if items, more, cursor, isList := listShape(obj); isList {
		return renderListTable(ctx, items, more, cursor, fields)
	}
	return renderResourceView(ctx, obj, fields)
}

// listShape detects the conventional pagination envelope:
// {"items": [...], "has_more": bool, "next_cursor": "..."?}.
func listShape(obj map[string]any) (items []any, hasMore bool, nextCursor string, ok bool) {
	raw, present := obj["items"]
	if !present {
		return nil, false, "", false
	}
	arr, isArr := raw.([]any)
	if !isArr {
		return nil, false, "", false
	}
	hm, _ := obj["has_more"].(bool)
	nc, _ := obj["next_cursor"].(string)
	return arr, hm, nc, true
}

// preferredColumnOrder is consulted before alphabetical ordering when picking
// which fields to surface in a list table. Keeps human-relevant identifiers
// to the left.
var preferredColumnOrder = []string{
	"id", "handle", "name", "status", "state", "kind", "type",
	"version", "latest_version", "queue", "queues",
	"created_at", "updated_at",
}

// renderListTable picks a stable set of scalar columns from the first row
// and renders the page as a tui table. When explicitFields is non-nil, those
// fields become the columns instead — that's the --fields projection.
func renderListTable(ctx *cli.Context, items []any, hasMore bool, nextCursor string, explicitFields []string) error {
	if len(items) == 0 {
		fmt.Fprintln(ctx.Stdout(), "(no results)")
		return nil
	}
	cols := explicitFields
	if len(cols) == 0 {
		cols = pickColumns(items)
	}
	header := make([]tui.TableColumn, len(cols))
	for i, c := range cols {
		header[i] = tui.TableColumn{Title: c}
	}
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		row := make([]string, len(cols))
		obj, _ := it.(map[string]any)
		for i, c := range cols {
			row[i] = formatCell(obj[c])
		}
		rows = append(rows, row)
	}
	selected := -1 // non-interactive: no row highlighted
	view := tui.Table(header, &selected).Rows(rows)
	footer := fmt.Sprintf("%d shown", len(items))
	if hasMore {
		footer += " — more available"
		if nextCursor != "" {
			footer += " (--cursor " + truncate(nextCursor, 16) + ")"
		}
	}
	stack := tui.Stack(view, tui.Text("%s", footer).Dim()).Gap(1)
	if err := tui.Print(stack, tui.PrintConfig{Output: ctx.Stdout()}); err != nil {
		return err
	}
	fmt.Fprintln(ctx.Stdout())
	return nil
}

// pickColumns chooses up to 6 columns from the first row's scalar fields,
// honoring preferredColumnOrder. Non-scalar fields (objects/arrays) are
// excluded — they render badly in a fixed-width table.
func pickColumns(items []any) []string {
	first, ok := items[0].(map[string]any)
	if !ok || len(first) == 0 {
		return []string{"value"}
	}
	scalars := map[string]bool{}
	for k, v := range first {
		if isScalar(v) {
			scalars[k] = true
		}
	}
	picked := []string{}
	seen := map[string]bool{}
	for _, name := range preferredColumnOrder {
		if scalars[name] {
			picked = append(picked, name)
			seen[name] = true
		}
		if len(picked) >= 6 {
			return picked
		}
	}
	remaining := make([]string, 0, len(scalars))
	for k := range scalars {
		if !seen[k] {
			remaining = append(remaining, k)
		}
	}
	sort.Strings(remaining)
	for _, k := range remaining {
		if len(picked) >= 6 {
			break
		}
		picked = append(picked, k)
	}
	if len(picked) == 0 {
		picked = append(picked, "value")
	}
	return picked
}

// renderResourceView renders a single resource as a key/value stack.
// Scalar fields render directly; nested objects/arrays show a count hint so
// the user knows there's more available via --output json. When
// explicitFields is non-nil, only those fields render — preserving the
// caller's order from --fields.
func renderResourceView(ctx *cli.Context, obj map[string]any, explicitFields []string) error {
	keys := explicitFields
	if len(keys) == 0 {
		keys = orderedKeys(obj)
	}
	rows := make([]tui.View, 0, len(keys))
	for _, k := range keys {
		v := obj[k]
		rows = append(rows, tui.KeyValue(k, formatCell(v)))
	}
	if len(rows) == 0 {
		fmt.Fprintln(ctx.Stdout(), "(empty)")
		return nil
	}
	view := tui.Stack(rows...)
	if err := tui.Print(view, tui.PrintConfig{Output: ctx.Stdout()}); err != nil {
		return err
	}
	fmt.Fprintln(ctx.Stdout())
	return nil
}

// orderedKeys returns object keys in preferredColumnOrder first, then the
// rest alphabetically. Stable across calls so tests can assert on output.
func orderedKeys(obj map[string]any) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, name := range preferredColumnOrder {
		if _, ok := obj[name]; ok {
			out = append(out, name)
			seen[name] = true
		}
	}
	rest := make([]string, 0, len(obj))
	for k := range obj {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func isScalar(v any) bool {
	switch v.(type) {
	case nil, string, bool, float64, int, int64, json.Number:
		return true
	}
	return false
}

// formatCell turns a JSON value into a single-line string suitable for a
// table cell or KeyValue value. Nested objects/arrays collapse to a hint.
func formatCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "—"
	case string:
		return truncate(x, 60)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		// Whole numbers render without a trailing ".0".
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case json.Number:
		return string(x)
	case []any:
		return fmt.Sprintf("[%d]", len(x))
	case map[string]any:
		return fmt.Sprintf("{%d fields}", len(x))
	default:
		return fmt.Sprintf("%v", x)
	}
}

func truncate(s string, max int) string {
	if max <= 1 || len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// printJSONFallback pretty-prints the body as JSON when the pretty renderer
// can't find a structured shape it understands.
func printJSONFallback(ctx *cli.Context, body []byte) error {
	var tmp any
	if err := json.Unmarshal(body, &tmp); err != nil {
		fmt.Fprintln(ctx.Stdout(), strings.TrimRight(string(body), "\n"))
		return nil
	}
	pretty, err := json.MarshalIndent(tmp, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(ctx.Stdout(), string(pretty))
	return nil
}
