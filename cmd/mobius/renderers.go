package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/tui"
)

// ResponseRenderer renders a successful response body for a specific
// operation. Implementations write to ctx.Stdout(); they are invoked only
// when the resolved --output mode is "pretty" (TTY default), so the
// canonical machine-parseable shape (json/yaml/id/name) is preserved for
// scripts.
//
// A renderer that decides the body is too unusual to handle nicely can call
// renderPretty(ctx, body) to fall through to the generic shape-driven path.
type ResponseRenderer func(ctx *cli.Context, body []byte) error

// responseRenderers maps OpenAPI operationId → custom pretty renderer. The
// generator looks the operationId up here before dispatching to the generic
// renderer; populate via RegisterResponseRenderer at process init.
var responseRenderers = map[string]ResponseRenderer{}

// RegisterResponseRenderer attaches a pretty renderer to one operation,
// keyed by its OpenAPI operationId (e.g. "getRun", "listWorkflows"). Hand-
// written sibling files call this from init() to layer custom views on top
// of the generic table/key-value renderer without touching cligen.
func RegisterResponseRenderer(opID string, fn ResponseRenderer) {
	responseRenderers[opID] = fn
}

// responseRendererFor returns the renderer registered for opID, or nil if
// none is registered. Called by the generated printResponse runtime.
func responseRendererFor(opID string) ResponseRenderer {
	return responseRenderers[opID]
}

func init() {
	RegisterResponseRenderer("getRun", renderRunDetail)
}

// renderRunDetail renders a WorkflowRunDetail as a status header followed by
// a per-path execution table. Falls through to the generic renderer when
// the response shape doesn't match (e.g. an unexpected error envelope or a
// schema we no longer recognize), so adding fields to the spec doesn't
// break the command.
func renderRunDetail(ctx *cli.Context, body []byte) error {
	var run map[string]any
	if err := json.Unmarshal(body, &run); err != nil {
		return renderPretty(ctx, body)
	}
	status, _ := run["status"].(string)
	if status == "" {
		return renderPretty(ctx, body)
	}

	header := tui.Stack(
		tui.KeyValue("workflow", asString(run["workflow_name"])),
		tui.KeyValue("run id", asString(run["id"])),
		tui.KeyValue("status", colorizeRunStatus(status)),
		tui.KeyValue("attempt", asString(run["attempt"])),
		tui.KeyValue("queue", orDash(asString(run["queue"]))),
	)

	views := []tui.View{header}
	if errMsg := asString(run["error_message"]); errMsg != "" {
		views = append(views,
			tui.Text("error: %s", truncate(errMsg, 200)).Fg(tui.ColorRed),
		)
	}

	if counts, ok := run["path_counts"].(map[string]any); ok {
		summary := fmt.Sprintf(
			"paths: %s total · %s working · %s waiting · %s completed · %s failed",
			asString(counts["total"]),
			asString(counts["working"]),
			asString(counts["waiting"]),
			asString(counts["completed"]),
			asString(counts["failed"]),
		)
		views = append(views, tui.Text("%s", summary).Dim())
	}

	if pathView := buildPathTable(run["paths"]); pathView != nil {
		views = append(views, pathView)
	}

	stack := tui.Stack(views...).Gap(1)
	if err := tui.Print(stack, tui.PrintConfig{Output: ctx.Stdout()}); err != nil {
		return err
	}
	fmt.Fprintln(ctx.Stdout())
	return nil
}

// buildPathTable renders the per-execution-path slice as a small table.
// Returns nil when there are no paths so the caller can omit the section.
func buildPathTable(rawPaths any) tui.View {
	paths, ok := rawPaths.([]any)
	if !ok || len(paths) == 0 {
		return nil
	}
	cols := []tui.TableColumn{
		{Title: "path"},
		{Title: "state"},
		{Title: "detail"},
	}
	rows := make([][]string, 0, len(paths))
	// Stable order so two consecutive `runs get` calls for the same run
	// produce identical output.
	sortedPaths := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		if obj, ok := p.(map[string]any); ok {
			sortedPaths = append(sortedPaths, obj)
		}
	}
	sort.Slice(sortedPaths, func(i, j int) bool {
		return asString(sortedPaths[i]["path_id"]) < asString(sortedPaths[j]["path_id"])
	})
	for _, p := range sortedPaths {
		rows = append(rows, []string{
			truncate(asString(p["path_id"]), 36),
			asString(p["state"]),
			pathDetail(p),
		})
	}
	selected := -1
	return tui.Table(cols, &selected).Rows(rows)
}

// pathDetail picks the most useful single-line description for a path, in
// priority order: error → wait reason → "—".
func pathDetail(p map[string]any) string {
	if msg := asString(p["error_message"]); msg != "" {
		return truncate(msg, 60)
	}
	if w, ok := p["waiting_on"].(map[string]any); ok && len(w) > 0 {
		kind := asString(w["kind"])
		reason := asString(w["reason"])
		if reason != "" {
			return truncate(kind+": "+reason, 60)
		}
		if kind != "" {
			return kind
		}
	}
	return "—"
}

// colorizeRunStatus picks a single ANSI color per terminal state. Plain
// string is returned when no styled text is wired (the tui.Text inside
// KeyValue would need a value-style wrapper to honor color; we settle for
// labelling textually).
func colorizeRunStatus(status string) string {
	// tui.KeyValue doesn't take a styled value, so we encode the visual cue
	// as glyphs that don't disappear on a plain terminal.
	switch strings.ToLower(status) {
	case "succeeded", "completed":
		return "✓ " + status
	case "failed", "cancelled", "timed_out":
		return "✗ " + status
	case "running", "working":
		return "⟳ " + status
	case "waiting", "paused", "sleeping", "retrying":
		return "⏸ " + status
	default:
		return status
	}
}

func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case json.Number:
		return string(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
