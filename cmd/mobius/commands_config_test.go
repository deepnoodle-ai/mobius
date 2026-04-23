package main

import (
	"reflect"
	"testing"
)

func TestSplitDottedConfig(t *testing.T) {
	tests := []struct {
		name     string
		entry    string
		wantCat  string
		wantKey  string
		wantVal  any
		wantErr  bool
	}{
		{name: "duration string", entry: "timeouts.wall_clock=30m", wantCat: "timeouts", wantKey: "wall_clock", wantVal: "30m"},
		{name: "never sentinel", entry: "timeouts.execution=never", wantCat: "timeouts", wantKey: "execution", wantVal: "never"},
		{name: "json null", entry: "timeouts.execution=null", wantCat: "timeouts", wantKey: "execution", wantVal: nil},
		{name: "json number", entry: "retry.max_attempts=3", wantCat: "retry", wantKey: "max_attempts", wantVal: float64(3)},
		{name: "json bool", entry: "flags.enabled=true", wantCat: "flags", wantKey: "enabled", wantVal: true},
		{name: "json string stays string", entry: `timeouts.claim="5m"`, wantCat: "timeouts", wantKey: "claim", wantVal: "5m"},
		{name: "empty value ok", entry: "timeouts.claim=", wantCat: "timeouts", wantKey: "claim", wantVal: ""},
		{name: "value with equals", entry: "meta.query=a=b", wantCat: "meta", wantKey: "query", wantVal: "a=b"},

		{name: "missing equals", entry: "timeouts.wall_clock", wantErr: true},
		{name: "missing dot", entry: "wall_clock=30m", wantErr: true},
		{name: "empty category", entry: ".wall_clock=30m", wantErr: true},
		{name: "empty key", entry: "timeouts.=30m", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, key, val, err := splitDottedConfig(tt.entry)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (cat=%q key=%q val=%v)", cat, key, val)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cat != tt.wantCat || key != tt.wantKey {
				t.Errorf("path: got (%q, %q), want (%q, %q)", cat, key, tt.wantCat, tt.wantKey)
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
			"timeouts.wall_clock=30m",
			"timeouts.claim=5m",
		})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := map[string]any{
			"timeouts": map[string]any{"wall_clock": "30m", "claim": "5m"},
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
timeouts:
  wall_clock: 1h
  claim: 5m
`)
		got, err := mergeConfigInput(yaml, []string{"timeouts.wall_clock=30m"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		tc := got["timeouts"].(map[string]any)
		if tc["wall_clock"] != "30m" {
			t.Errorf("flag should override file: got %#v", tc["wall_clock"])
		}
		if tc["claim"] != "5m" {
			t.Errorf("untouched file key should survive: got %#v", tc["claim"])
		}
	})

	t.Run("flags merge into existing category from file", func(t *testing.T) {
		yaml := []byte("timeouts:\n  claim: 5m\n")
		got, err := mergeConfigInput(yaml, []string{"timeouts.liveness=90s"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		tc := got["timeouts"].(map[string]any)
		if tc["claim"] != "5m" || tc["liveness"] != "90s" {
			t.Errorf("merge incomplete: %#v", tc)
		}
	})

	t.Run("flags can create a new category not in file", func(t *testing.T) {
		yaml := []byte("timeouts:\n  claim: 5m\n")
		got, err := mergeConfigInput(yaml, []string{"retry.max_attempts=3"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if _, ok := got["retry"].(map[string]any); !ok {
			t.Errorf("retry category should be created: %#v", got)
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
