package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"
	"gopkg.in/yaml.v3"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerConfigExtensions layers cascade-aware flags and commands on top of
// the generated CLI. It augments `runs start` with --config / --config-file,
// adds `--show` to `runs get`, and adds `projects set-config` +
// `projects clear-config`. The wire shape is a flat list of ConfigEntries
// (category/key/value); nested maps from --config-file are flattened before
// the request is sent. The server does no merging on `runs start`, so
// set-config reads the current config, overlays the new entries, and PUTs
// the result.
func registerConfigExtensions(app *cli.App) {
	runsGrp := app.Group("runs")

	startCmd := runsGrp.Command("start")
	startCmd.Flags(
		cli.String("definition-id", "").Help("ID of an existing workflow definition to run (saved mode)."),
		cli.String("external-id", "").Help("Caller-supplied idempotency key or correlation ID attached to the run."),
		cli.String("inputs", "").Help("Input values for the workflow (JSON object)."),
		cli.String("metadata", "").Help("Caller-supplied string metadata attached to the run (JSON object)."),
		cli.String("queue", "").Help("Queue name to enqueue the run on. Defaults to \"default\"."),
		cli.String("spec", "").Help("Inline workflow spec (JSON). Switches the request to inline mode."),
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
		Description("Set cascade config values on a project. Reads current config, overlays the supplied entries, and PUTs the result.").
		Args("id").
		Flags(
			cli.Strings("config", "").Help("Dotted path: <category>.<key>=<value>. Repeatable. Set value to null to remove the key."),
			cli.String("config-file", "").Help("YAML/JSON file containing a nested config object; merged client-side with --config."),
		).
		Use(requireAuth()).
		Run(projectsSetConfigHandler)

	projectsGrp.Command("clear-config").
		Description("Clear a cascade category on a project (drops every stored entry whose category matches, so the project inherits from service defaults for that category).").
		Args("id").
		Flags(
			cli.String("category", "").Required().Help("Category to clear (e.g. `timeouts`)."),
		).
		Use(requireAuth()).
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
// merged cascade input as a nested map. Dotted-path flags override file keys
// on conflict.
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
// a string unless it parses as a JSON scalar (null, number, bool, quoted string)
// — the server parses and validates it per category.
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
// go-yaml for nested maps) into map[string]any so the result walks cleanly
// when we flatten it to ConfigEntries.
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

// configKey is a flat (category, key) coordinate used to identify an entry
// for removal or overlay.
type configKey struct {
	Category string
	Key      string
}

// flattenConfigInput turns a nested cascade map (as produced by
// parseConfigInput) into a flat list of ConfigEntries plus a set of keys
// whose value was explicitly null. The entry list is sorted by (category,
// key) so output is deterministic.
//
// Rejects non-scalar values (lists, maps, objects) under a key: the server
// expects strings keyed under (category, key). Stringifies bools and numbers
// using the Go default formatting.
func flattenConfigInput(in map[string]any) ([]api.ConfigEntry, []configKey, error) {
	var entries []api.ConfigEntry
	var removals []configKey
	categories := make([]string, 0, len(in))
	for cat := range in {
		categories = append(categories, cat)
	}
	sort.Strings(categories)
	for _, cat := range categories {
		raw, ok := in[cat].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("config category %q: expected object, got %T", cat, in[cat])
		}
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := raw[k]
			if v == nil {
				removals = append(removals, configKey{Category: cat, Key: k})
				continue
			}
			s, err := stringifyConfigValue(v)
			if err != nil {
				return nil, nil, fmt.Errorf("config %s.%s: %w", cat, k, err)
			}
			entries = append(entries, api.ConfigEntry{Category: cat, Key: k, Value: s})
		}
	}
	return entries, removals, nil
}

func stringifyConfigValue(v any) (string, error) {
	switch val := v.(type) {
	case string:
		return val, nil
	case bool:
		return strconv.FormatBool(val), nil
	case int:
		return strconv.Itoa(val), nil
	case int64:
		return strconv.FormatInt(val, 10), nil
	case uint:
		return strconv.FormatUint(uint64(val), 10), nil
	case uint64:
		return strconv.FormatUint(val, 10), nil
	case float64:
		// json.Unmarshal/yaml numeric literals land here. Emit without a
		// decimal point when the value is integral so "3" stays "3".
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10), nil
		}
		return strconv.FormatFloat(val, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("unsupported value type %T", v)
	}
}

func runsStartWithConfigHandler(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	p0 := authFor(ctx).Project

	// Build cascade config entries first so a bad --config fails before we
	// bother picking a mode.
	var configEntries *api.ConfigEntries
	if ctx.IsSet("config") || ctx.IsSet("config-file") {
		nested, err := parseConfigInput(ctx)
		if err != nil {
			return err
		}
		entries, removals, err := flattenConfigInput(nested)
		if err != nil {
			return err
		}
		if len(removals) > 0 {
			return fmt.Errorf("--config with null value is not supported on `runs start`; use `projects set-config` to remove stored entries")
		}
		if len(entries) > 0 {
			ce := api.ConfigEntries(entries)
			configEntries = &ce
		}
	}

	flagSet := ctx.String("file") != "" ||
		ctx.IsSet("definition-id") || ctx.IsSet("external-id") ||
		ctx.IsSet("inputs") || ctx.IsSet("metadata") ||
		ctx.IsSet("queue") || ctx.IsSet("spec") ||
		ctx.IsSet("config") || ctx.IsSet("config-file")
	if !flagSet {
		return fmt.Errorf("at least one flag or --file is required")
	}

	var body api.StartRunJSONRequestBody
	if err := readJSONBody(ctx, &body); err != nil {
		return err
	}

	// Pick the mode. --spec opts into inline; anything else uses saved. If
	// --file already set a union and the caller provided neither flag, keep
	// whatever --file supplied and let the server validate it.
	switch {
	case ctx.IsSet("spec"):
		inline, _ := body.AsStartInlineRunRequest()
		inline.Mode = "inline"
		if ctx.IsSet("external-id") {
			v := ctx.String("external-id")
			inline.ExternalId = &v
		}
		if ctx.IsSet("inputs") {
			var v map[string]any
			if err := json.Unmarshal([]byte(ctx.String("inputs")), &v); err != nil {
				return fmt.Errorf("--inputs: invalid JSON: %w", err)
			}
			inline.Inputs = &v
		}
		if ctx.IsSet("metadata") {
			var v map[string]string
			if err := json.Unmarshal([]byte(ctx.String("metadata")), &v); err != nil {
				return fmt.Errorf("--metadata: invalid JSON: %w", err)
			}
			inline.Metadata = &v
		}
		if ctx.IsSet("queue") {
			v := ctx.String("queue")
			inline.Queue = &v
		}
		if err := json.Unmarshal([]byte(ctx.String("spec")), &inline.Spec); err != nil {
			return fmt.Errorf("--spec: invalid JSON: %w", err)
		}
		if configEntries != nil {
			inline.Config = configEntries
		}
		if err := body.FromStartInlineRunRequest(inline); err != nil {
			return fmt.Errorf("build inline run request: %w", err)
		}
	case ctx.IsSet("definition-id") || ctx.IsSet("external-id") ||
		ctx.IsSet("inputs") || ctx.IsSet("metadata") ||
		ctx.IsSet("queue") || configEntries != nil:
		saved, _ := body.AsStartSavedRunRequest()
		saved.Mode = "saved"
		if ctx.IsSet("definition-id") {
			saved.DefinitionId = ctx.String("definition-id")
		}
		if ctx.IsSet("external-id") {
			v := ctx.String("external-id")
			saved.ExternalId = &v
		}
		if ctx.IsSet("inputs") {
			var v map[string]any
			if err := json.Unmarshal([]byte(ctx.String("inputs")), &v); err != nil {
				return fmt.Errorf("--inputs: invalid JSON: %w", err)
			}
			saved.Inputs = &v
		}
		if ctx.IsSet("metadata") {
			var v map[string]string
			if err := json.Unmarshal([]byte(ctx.String("metadata")), &v); err != nil {
				return fmt.Errorf("--metadata: invalid JSON: %w", err)
			}
			saved.Metadata = &v
		}
		if ctx.IsSet("queue") {
			v := ctx.String("queue")
			saved.Queue = &v
		}
		if configEntries != nil {
			saved.Config = configEntries
		}
		if saved.DefinitionId == "" {
			return fmt.Errorf("--definition-id is required (or supply it via --file)")
		}
		if err := body.FromStartSavedRunRequest(saved); err != nil {
			return fmt.Errorf("build saved run request: %w", err)
		}
	}

	resp, err := client.StartRunWithResponse(ctx.Context(), p0, body)
	if err != nil {
		return err
	}
	return printResponse(ctx, "startRun", resp.StatusCode(), resp.Body)
}

func runsGetWithConfigHandler(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	p0 := authFor(ctx).Project
	p1 := ctx.Arg(0)
	resp, err := client.GetRunWithResponse(ctx.Context(), p0, p1)
	if err != nil {
		return err
	}
	show := ctx.String("show")
	if show == "" || resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return printResponse(ctx, "getRun", resp.StatusCode(), resp.Body)
	}
	var envelope map[string]any
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return printResponse(ctx, "getRun", resp.StatusCode(), resp.Body)
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
	nested, err := parseConfigInput(ctx)
	if err != nil {
		return err
	}
	overlay, removals, err := flattenConfigInput(nested)
	if err != nil {
		return err
	}
	if len(overlay) == 0 && len(removals) == 0 {
		return fmt.Errorf("at least one --config or --config-file is required")
	}

	// The server replaces the project config wholesale, so read the current
	// entries and merge in our overlay before writing back.
	existing, err := client.GetProjectConfigWithResponse(ctx.Context(), id)
	if err != nil {
		return err
	}
	if existing.StatusCode() < 200 || existing.StatusCode() >= 300 {
		return printResponse(ctx, "getProjectConfig", existing.StatusCode(), existing.Body)
	}

	merged := mergeConfigEntries(existingEntries(existing.JSON200), overlay, removals)
	body := api.UpdateProjectConfigJSONRequestBody(merged)
	resp, err := client.UpdateProjectConfigWithResponse(ctx.Context(), id, body)
	if err != nil {
		return err
	}
	return printResponse(ctx, "updateProjectConfig", resp.StatusCode(), resp.Body)
}

func projectsClearConfigHandler(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	id := ctx.Arg(0)
	category := ctx.String("category")

	existing, err := client.GetProjectConfigWithResponse(ctx.Context(), id)
	if err != nil {
		return err
	}
	if existing.StatusCode() < 200 || existing.StatusCode() >= 300 {
		return printResponse(ctx, "getProjectConfig", existing.StatusCode(), existing.Body)
	}

	kept := dropCategory(existingEntries(existing.JSON200), category)
	body := api.UpdateProjectConfigJSONRequestBody(kept)
	resp, err := client.UpdateProjectConfigWithResponse(ctx.Context(), id, body)
	if err != nil {
		return err
	}
	return printResponse(ctx, "updateProjectConfig", resp.StatusCode(), resp.Body)
}

func existingEntries(p *api.ConfigEntries) []api.ConfigEntry {
	if p == nil {
		return nil
	}
	return []api.ConfigEntry(*p)
}

// mergeConfigEntries overlays `overlay` on top of `base`, keyed by (category,
// key). Entries listed in `removals` are dropped from the result. Order is
// sorted by (category, key) so the PUT payload is deterministic.
func mergeConfigEntries(base, overlay []api.ConfigEntry, removals []configKey) []api.ConfigEntry {
	byKey := map[configKey]api.ConfigEntry{}
	for _, e := range base {
		byKey[configKey{Category: e.Category, Key: e.Key}] = e
	}
	for _, e := range overlay {
		byKey[configKey{Category: e.Category, Key: e.Key}] = e
	}
	for _, k := range removals {
		delete(byKey, k)
	}
	out := make([]api.ConfigEntry, 0, len(byKey))
	for _, e := range byKey {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func dropCategory(base []api.ConfigEntry, category string) []api.ConfigEntry {
	out := make([]api.ConfigEntry, 0, len(base))
	for _, e := range base {
		if e.Category == category {
			continue
		}
		out = append(out, e)
	}
	return out
}
