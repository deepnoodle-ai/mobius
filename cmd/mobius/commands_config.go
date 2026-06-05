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

// registerConfigExtensions layers config-aware project commands on top of the
// generated CLI. The wire shape is a flat list of ConfigEntries (key/value);
// nested maps from --config-file are flattened before the request is sent.
func registerConfigExtensions(app *cli.App) {
	projectsGrp := app.Group("projects")

	projectsGrp.Command("set-config").
		Description("Set config values on a project. Reads current config, overlays the supplied entries, and PUTs the result.").
		AddArg(&cli.Arg{Name: "id", Description: "Project ID.", Required: true}).
		Flags(
			cli.Strings("config", "").Help("Dotted key: <key>=<value>. Repeatable. Set value to null to remove the key."),
			cli.String("config-file", "").Help("YAML/JSON file containing a nested config object; merged client-side with --config."),
		).
		Use(requireAuth()).
		Run(projectsSetConfigHandler)

	projectsGrp.Command("clear-config").
		Description("Clear project config, or entries under a key prefix. Without --key-prefix this calls DELETE and clears everything.").
		AddArg(&cli.Arg{Name: "id", Description: "Project ID.", Required: true}).
		Flags(
			cli.String("key-prefix", "").Help("Optional key prefix to clear (e.g. `jobs.timeouts.`)."),
		).
		Use(requireAuth()).
		Run(projectsClearConfigHandler)
}

// parseConfigInput merges optional --config-file and repeated --config flags
// into a single config object. Dotted-key flags override file keys.
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
// file bytes (YAML or JSON) and a list of dotted-key flags, it returns the
// merged config input as a nested map. Dotted-key flags override file keys
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
		key, val, err := splitDottedConfig(entry)
		if err != nil {
			return nil, err
		}
		setNestedConfigKey(out, key, val)
	}
	return out, nil
}

// splitDottedConfig parses a "<key>=<value>" flag. The value is left as
// a string unless it parses as a JSON scalar (null, number, bool, quoted string)
// — the server parses and validates it per key.
func splitDottedConfig(entry string) (key string, value any, err error) {
	eq := strings.IndexByte(entry, '=')
	if eq < 0 {
		return "", nil, fmt.Errorf("--config %q: expected <key>=<value>", entry)
	}
	key, raw := entry[:eq], entry[eq+1:]
	dot := strings.IndexByte(key, '.')
	if dot < 0 {
		return "", nil, fmt.Errorf("--config %q: key must be dotted (e.g. runs.timeouts.execution=30m)", entry)
	}
	if key[:dot] == "" || key[dot+1:] == "" {
		return "", nil, fmt.Errorf("--config %q: empty key segment", entry)
	}
	// Accept JSON scalars so callers can pass `null`, numbers, or booleans.
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		value = parsed
	} else {
		value = raw
	}
	return key, value, nil
}

func setNestedConfigKey(out map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
	cursor := out
	for _, part := range parts[:len(parts)-1] {
		next, _ := cursor[part].(map[string]any)
		if next == nil {
			next = map[string]any{}
			cursor[part] = next
		}
		cursor = next
	}
	cursor[parts[len(parts)-1]] = value
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

// configKey is a flat key coordinate used to identify an entry for removal
// or overlay.
type configKey struct {
	Key string
}

// flattenConfigInput turns a nested config map (as produced by
// parseConfigInput) into a flat list of ConfigEntries plus a set of keys
// whose value was explicitly null. The entry list is sorted by key.
//
// Rejects non-scalar values (lists, maps, objects) under a key: the server
// expects string values for each registered key. Stringifies bools and numbers
// using the Go default formatting.
func flattenConfigInput(in map[string]any) ([]api.ConfigEntry, []configKey, error) {
	var entries []api.ConfigEntry
	var removals []configKey
	if err := flattenConfigKey(&entries, &removals, "", in); err != nil {
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	sort.Slice(removals, func(i, j int) bool { return removals[i].Key < removals[j].Key })
	return entries, removals, nil
}

func flattenConfigKey(entries *[]api.ConfigEntry, removals *[]configKey, prefix string, raw any) error {
	if raw == nil {
		if prefix == "" {
			return nil
		}
		*removals = append(*removals, configKey{Key: prefix})
		return nil
	}
	if m, ok := raw.(map[string]any); ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			if err := flattenConfigKey(entries, removals, key, m[k]); err != nil {
				return err
			}
		}
		return nil
	}
	if prefix == "" {
		return fmt.Errorf("config root: expected object, got %T", raw)
	}
	s, err := stringifyConfigValue(raw)
	if err != nil {
		return fmt.Errorf("config %s: %w", prefix, err)
	}
	*entries = append(*entries, api.ConfigEntry{Key: prefix, Value: s})
	return nil
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
	prefix := ctx.String("key-prefix")
	if prefix == "" {
		resp, err := client.DeleteProjectConfigWithResponse(ctx.Context(), id)
		if err != nil {
			return err
		}
		return printResponse(ctx, "deleteProjectConfig", resp.StatusCode(), resp.Body)
	}

	existing, err := client.GetProjectConfigWithResponse(ctx.Context(), id)
	if err != nil {
		return err
	}
	if existing.StatusCode() < 200 || existing.StatusCode() >= 300 {
		return printResponse(ctx, "getProjectConfig", existing.StatusCode(), existing.Body)
	}

	kept := dropKeyPrefix(existingEntries(existing.JSON200), prefix)
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

// mergeConfigEntries overlays `overlay` on top of `base`, keyed by config key.
// Entries listed in `removals` are dropped from the result. Order is sorted
// by key so the PUT payload is deterministic.
func mergeConfigEntries(base, overlay []api.ConfigEntry, removals []configKey) []api.ConfigEntry {
	byKey := map[configKey]api.ConfigEntry{}
	for _, e := range base {
		byKey[configKey{Key: e.Key}] = e
	}
	for _, e := range overlay {
		byKey[configKey{Key: e.Key}] = e
	}
	for _, k := range removals {
		delete(byKey, k)
	}
	out := make([]api.ConfigEntry, 0, len(byKey))
	for _, e := range byKey {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out
}

func dropKeyPrefix(base []api.ConfigEntry, prefix string) []api.ConfigEntry {
	out := make([]api.ConfigEntry, 0, len(base))
	for _, e := range base {
		if strings.HasPrefix(e.Key, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}
