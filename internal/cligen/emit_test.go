package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmitStringSliceValue(t *testing.T) {
	t.Run("built in string", func(t *testing.T) {
		var b bytes.Buffer
		emitStringSliceValue(&b, "\t", "v", "[]string", `ctx.Strings("status")`)
		if got, want := b.String(), "\tv := ctx.Strings(\"status\")\n"; got != want {
			t.Fatalf("unexpected output:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("named string enum", func(t *testing.T) {
		var b bytes.Buffer
		emitStringSliceValue(&b, "\t", "v", "[]SessionNudgeStatus", `ctx.Strings("status")`)
		got := b.String()
		for _, want := range []string{
			`raw := ctx.Strings("status")`,
			`v := make([]api.SessionNudgeStatus, len(raw))`,
			`for i, item := range raw { v[i] = api.SessionNudgeStatus(item) }`,
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("output %q does not contain %q", got, want)
			}
		}
	})
}
