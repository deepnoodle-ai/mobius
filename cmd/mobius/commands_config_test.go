package main

import (
	"reflect"
	"testing"
)

func TestSplitDottedConfig(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		wantKey string
		wantVal any
		wantErr bool
	}{
		{name: "duration string", entry: "runs.timeouts.execution=30m", wantKey: "runs.timeouts.execution", wantVal: "30m"},
		{name: "never sentinel", entry: "jobs.timeouts.execution=never", wantKey: "jobs.timeouts.execution", wantVal: "never"},
		{name: "json null", entry: "jobs.timeouts.execution=null", wantKey: "jobs.timeouts.execution", wantVal: nil},
		{name: "json number", entry: "retry.max_attempts=3", wantKey: "retry.max_attempts", wantVal: float64(3)},
		{name: "json bool", entry: "flags.enabled=true", wantKey: "flags.enabled", wantVal: true},
		{name: "json string stays string", entry: `jobs.timeouts.claim="5m"`, wantKey: "jobs.timeouts.claim", wantVal: "5m"},
		{name: "empty value ok", entry: "jobs.timeouts.claim=", wantKey: "jobs.timeouts.claim", wantVal: ""},
		{name: "value with equals", entry: "meta.query=a=b", wantKey: "meta.query", wantVal: "a=b"},

		{name: "missing equals", entry: "runs.timeouts.execution", wantErr: true},
		{name: "missing dot", entry: "wall_clock=30m", wantErr: true},
		{name: "empty first segment", entry: ".wall_clock=30m", wantErr: true},
		{name: "empty last segment", entry: "timeouts.=30m", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, val, err := splitDottedConfig(tt.entry)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (key=%q val=%v)", key, val)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if key != tt.wantKey {
				t.Errorf("key: got %q, want %q", key, tt.wantKey)
			}
			if !reflect.DeepEqual(val, tt.wantVal) {
				t.Errorf("value: got %#v, want %#v", val, tt.wantVal)
			}
		})
	}
}

func TestMergeConfigInput(t *testing.T) {
	t.Run("flags only", func(t *testing.T) {
		got, err := mergeConfigInput(nil, []string{
			"runs.timeouts.execution=30m",
			"jobs.timeouts.claim=5m",
		})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := map[string]any{
			"runs": map[string]any{"timeouts": map[string]any{"execution": "30m"}},
			"jobs": map[string]any{"timeouts": map[string]any{"claim": "5m"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("yaml file only, nested map normalised", func(t *testing.T) {
		yaml := []byte(`
timeouts:
  wall_clock: 30m
  claim: 5m
retry:
  max_attempts: 3
`)
		got, err := mergeConfigInput(yaml, nil)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// Every nested map MUST be map[string]any (not map[any]any) so that
		// json.Marshal produces a valid ConfigInput payload.
		tc, ok := got["timeouts"].(map[string]any)
		if !ok {
			t.Fatalf("timeouts: got %T, want map[string]any", got["timeouts"])
		}
		if tc["wall_clock"] != "30m" || tc["claim"] != "5m" {
			t.Errorf("timeouts contents: %#v", tc)
		}
		rc, ok := got["retry"].(map[string]any)
		if !ok {
			t.Fatalf("retry: got %T, want map[string]any", got["retry"])
		}
		// YAML unmarshals integers as int by default; the server accepts
		// either number shape for ConfigInput, so no coercion here.
		if rc["max_attempts"] != 3 {
			t.Errorf("retry.max_attempts: got %#v, want 3", rc["max_attempts"])
		}
	})

	t.Run("flags override file on key conflict", func(t *testing.T) {
		yaml := []byte(`
runs:
  timeouts:
    execution: 1h
jobs:
  timeouts:
    claim: 5m
`)
		got, err := mergeConfigInput(yaml, []string{"runs.timeouts.execution=30m"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		runs := got["runs"].(map[string]any)
		runTimeouts := runs["timeouts"].(map[string]any)
		if runTimeouts["execution"] != "30m" {
			t.Errorf("flag should override file: got %#v", runTimeouts["execution"])
		}
		jobs := got["jobs"].(map[string]any)
		jobTimeouts := jobs["timeouts"].(map[string]any)
		if jobTimeouts["claim"] != "5m" {
			t.Errorf("untouched file key should survive: got %#v", jobTimeouts["claim"])
		}
	})

	t.Run("flags merge into existing key object from file", func(t *testing.T) {
		yaml := []byte("jobs:\n  timeouts:\n    claim: 5m\n")
		got, err := mergeConfigInput(yaml, []string{"jobs.timeouts.heartbeat=90s"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		jobs := got["jobs"].(map[string]any)
		timeouts := jobs["timeouts"].(map[string]any)
		if timeouts["claim"] != "5m" || timeouts["heartbeat"] != "90s" {
			t.Errorf("merge incomplete: %#v", timeouts)
		}
	})

	t.Run("flags can create a new top-level object not in file", func(t *testing.T) {
		yaml := []byte("jobs:\n  timeouts:\n    claim: 5m\n")
		got, err := mergeConfigInput(yaml, []string{"retry.max_attempts=3"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if _, ok := got["retry"].(map[string]any); !ok {
			t.Errorf("retry object should be created: %#v", got)
		}
	})

	t.Run("invalid flag surfaces error", func(t *testing.T) {
		_, err := mergeConfigInput(nil, []string{"bad-no-equals"})
		if err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("invalid yaml surfaces error", func(t *testing.T) {
		_, err := mergeConfigInput([]byte("{bad: yaml: here"), nil)
		if err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("empty inputs returns empty map", func(t *testing.T) {
		got, err := mergeConfigInput(nil, nil)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v, want empty", got)
		}
	})
}

func TestStringifyConfigValue(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{name: "uint", in: uint(42), want: "42"},
		{name: "uint64", in: uint64(18446744073709551615), want: "18446744073709551615"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stringifyConfigValue(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeYAMLValue(t *testing.T) {
	t.Run("map[any]any becomes map[string]any", func(t *testing.T) {
		in := map[string]any{
			"outer": map[any]any{
				"inner": map[any]any{"k": "v"},
			},
		}
		got := normalizeYAMLMap(in)
		outer, ok := got["outer"].(map[string]any)
		if !ok {
			t.Fatalf("outer: got %T", got["outer"])
		}
		inner, ok := outer["inner"].(map[string]any)
		if !ok {
			t.Fatalf("inner: got %T", outer["inner"])
		}
		if inner["k"] != "v" {
			t.Errorf("inner.k: got %#v", inner["k"])
		}
	})

	t.Run("arrays recurse", func(t *testing.T) {
		in := map[string]any{
			"list": []any{
				map[any]any{"k": "v"},
				"plain",
			},
		}
		got := normalizeYAMLMap(in)
		list := got["list"].([]any)
		first, ok := list[0].(map[string]any)
		if !ok {
			t.Fatalf("list[0]: got %T", list[0])
		}
		if first["k"] != "v" {
			t.Errorf("list[0].k: got %#v", first["k"])
		}
		if list[1] != "plain" {
			t.Errorf("list[1]: got %#v", list[1])
		}
	})

	t.Run("non-string map keys stringified", func(t *testing.T) {
		in := map[string]any{
			"outer": map[any]any{42: "v"},
		}
		got := normalizeYAMLMap(in)
		outer := got["outer"].(map[string]any)
		if outer["42"] != "v" {
			t.Errorf("got %#v, want key \"42\"", outer)
		}
	})
}
