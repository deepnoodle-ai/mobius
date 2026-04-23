package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"
	"gopkg.in/yaml.v3"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerConfigExtensions layers cascade-aware flags and commands on top of
// the generated CLI. It augments `runs start` with --config / --config-file,
// adds `--show` to `runs get`, and adds `projects set-config` +
// `projects clear-config`. The shape of the input (and the handling of `null`
// to clear a category) matches PRD 035 exactly — the server does no merging.
func registerConfigExtensions(app *cli.App) {
	runsGrp := app.Group("runs")

	startCmd := runsGrp.Command("start")
	startCmd.Flags(
		cli.Strings("config", "").Help("Cascade config override as dotted path: <category>.<key>=<value>. Repeatable. Example: --config timeouts.wall_clock=30m"),
		cli.String("config-file", "").Help("Path to a YAML or JSON file containing a cascade config object (merged client-side with --config flags before send)."),
	)
	startCmd.Run(runsStartWithConfigHandler)

	getCmd := runsGrp.Command("get")
	getCmd.Flags(
		cli.String("show", "").Enum("resolved_config", "default_job_config").Help("Pretty-print a specific frozen cascade field from the response instead of the whole run."),
	)
	getCmd.Run(runsGetWithConfigHandler)

	projectsGrp := app.Group("projects")

	projectsGrp.Command("set-config").
		Description("Set cascade config values on a project (replaces the given categories).").
		Args("id").
		Flags(
			cli.Strings("config", "").Help("Dotted path: <category>.<key>=<value>. Repeatable."),
			cli.String("config-file", "").Help("YAML/JSON file containing a config object; merged client-side with --config."),
		).
		Use(cli.RequireFlags("api-key")).
		Run(projectsSetConfigHandler)

	projectsGrp.Command("clear-config").
		Description("Clear a cascade category on a project (sends `config: { <category>: null }` so the project inherits from service defaults).").
		Args("id").
		Flags(
			cli.String("category", "").Required().Help("Category to clear (e.g. `timeouts`)."),
		).
		Use(cli.RequireFlags("api-key")).
		Run(projectsClearConfigHandler)
}

// parseConfigInput merges optional --config-file and repeated --config flags
// into a single cascade config object. Dotted-path flags override file keys.
func parseConfigInput(ctx *cli.Context) (map[string]any, error) {
	var fileBytes []byte
	if path := ctx.String("config-file"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read --config-file: %w", err)
		}
		fileBytes = data
	}
	return mergeConfigInput(fileBytes, ctx.Strings("config"))
}

// mergeConfigInput is the pure-input core of parseConfigInput. Given optional
// file bytes (YAML or JSON) and a list of dotted-path flags, it returns the
// merged cascade input. Dotted-path flags override file keys on conflict.
func mergeConfigInput(fileBytes []byte, flags []string) (map[string]any, error) {
	out := map[string]any{}
	if len(fileBytes) > 0 {
		if err := yaml.Unmarshal(fileBytes, &out); err != nil {
			return nil, fmt.Errorf("parse --config-file: %w", err)
		}
		out = normalizeYAMLMap(out)
	}
	for _, entry := range flags {
		cat, key, val, err := splitDottedConfig(entry)
		if err != nil {
			return nil, err
		}
		catMap, _ := out[cat].(map[string]any)
		if catMap == nil {
			catMap = map[string]any{}
		}
		catMap[key] = val
		out[cat] = catMap
	}
	return out, nil
}

// splitDottedConfig parses a "<cat>.<key>=<value>" flag. The value is left as
// a string — the server parses and validates it per category.
func splitDottedConfig(entry string) (category, key string, value any, err error) {
	eq := strings.IndexByte(entry, '=')
	if eq < 0 {
		return "", "", nil, fmt.Errorf("--config %q: expected <category>.<key>=<value>", entry)
	}
	path, raw := entry[:eq], entry[eq+1:]
	dot := strings.IndexByte(path, '.')
	if dot < 0 {
		return "", "", nil, fmt.Errorf("--config %q: key must be dotted (e.g. timeouts.wall_clock=30m)", entry)
	}
	category = path[:dot]
	key = path[dot+1:]
	if category == "" || key == "" {
		return "", "", nil, fmt.Errorf("--config %q: empty category or key", entry)
	}
	// Accept JSON scalars so callers can pass `null`, numbers, or booleans.
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		value = parsed
	} else {
		value = raw
	}
	return category, key, value, nil
}

// normalizeYAMLMap converts map[interface{}]interface{} values (produced by
// go-yaml for nested maps) into map[string]any so the result marshals to JSON
// cleanly as a ConfigInput.
func normalizeYAMLMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = normalizeYAMLValue(v)
	}
	return out
}

func normalizeYAMLValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return normalizeYAMLMap(val)
	case map[any]any:
		m := make(map[string]any, len(val))
		for k, vv := range val {
			m[fmt.Sprint(k)] = normalizeYAMLValue(vv)
		}
		return m
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = normalizeYAMLValue(item)
		}
		return out
	default:
		return v
	}
}

func runsStartWithConfigHandler(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	p0 := ctx.String("project")
	var body api.StartRunJSONRequestBody
	if err := readJSONBody(ctx, &body); err != nil {
		return err
	}
	if ctx.IsSet("definition-id") {
		v := ctx.String("definition-id")
		body.DefinitionId = &v
	}
	if ctx.IsSet("external-id") {
		v := ctx.String("external-id")
		body.ExternalId = &v
	}
	if ctx.IsSet("inputs") {
		if err := json.Unmarshal([]byte(ctx.String("inputs")), &body.Inputs); err != nil {
			return fmt.Errorf("--inputs: invalid JSON: %w", err)
		}
	}
	if ctx.IsSet("metadata") {
		if err := json.Unmarshal([]byte(ctx.String("metadata")), &body.Metadata); err != nil {
			return fmt.Errorf("--metadata: invalid JSON: %w", err)
		}
	}
	if ctx.IsSet("queue") {
		v := ctx.String("queue")
		body.Queue = &v
	}
	if ctx.IsSet("spec") {
		if err := json.Unmarshal([]byte(ctx.String("spec")), &body.Spec); err != nil {
			return fmt.Errorf("--spec: invalid JSON: %w", err)
		}
	}
	if ctx.IsSet("config") || ctx.IsSet("config-file") {
		merged, err := parseConfigInput(ctx)
		if err != nil {
			return err
		}
		if len(merged) > 0 {
			ci := api.ConfigInput(merged)
			body.Config = &ci
		}
	}
	if ctx.String("file") == "" &&
		!ctx.IsSet("definition-id") && !ctx.IsSet("external-id") &&
		!ctx.IsSet("inputs") && !ctx.IsSet("metadata") &&
		!ctx.IsSet("queue") && !ctx.IsSet("spec") &&
		!ctx.IsSet("config") && !ctx.IsSet("config-file") {
		return fmt.Errorf("at least one flag or --file is required")
	}
	resp, err := client.StartRunWithResponse(ctx.Context(), p0, body)
	if err != nil {
		return err
	}
	return printResponse(ctx, resp.StatusCode(), resp.Body)
}

func runsGetWithConfigHandler(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	p0 := ctx.String("project")
	p1 := ctx.Arg(0)
	resp, err := client.GetRunWithResponse(ctx.Context(), p0, p1)
	if err != nil {
		return err
	}
	show := ctx.String("show")
	if show == "" || resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return printResponse(ctx, resp.StatusCode(), resp.Body)
	}
	var envelope map[string]any
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return printResponse(ctx, resp.StatusCode(), resp.Body)
	}
	sub, ok := envelope[show]
	if !ok || sub == nil {
		ctx.Println("(unset)")
		return nil
	}
	pretty, err := json.MarshalIndent(sub, "", "  ")
	if err != nil {
		return err
	}
	ctx.Println(string(pretty))
	return nil
}

func projectsSetConfigHandler(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	id := ctx.Arg(0)
	merged, err := parseConfigInput(ctx)
	if err != nil {
		return err
	}
	if len(merged) == 0 {
		return fmt.Errorf("at least one --config or --config-file is required")
	}
	ci := api.ConfigInput(merged)
	body := api.UpdateProjectJSONRequestBody{Config: &ci}
	resp, err := client.UpdateProjectWithResponse(ctx.Context(), id, body)
	if err != nil {
		return err
	}
	return printResponse(ctx, resp.StatusCode(), resp.Body)
}

func projectsClearConfigHandler(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	id := ctx.Arg(0)
	category := ctx.String("category")
	// Per FR-F9: `config: { <cat>: null }` clears that category.
	ci := api.ConfigInput(map[string]any{category: nil})
	body := api.UpdateProjectJSONRequestBody{Config: &ci}
	resp, err := client.UpdateProjectWithResponse(ctx.Context(), id, body)
	if err != nil {
		return err
	}
	return printResponse(ctx, resp.StatusCode(), resp.Body)
}
