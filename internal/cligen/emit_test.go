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

func TestInt64PathParamsAreSupported(t *testing.T) {
	if !isSimplePathParam("int64", &ClientInfo{}) {
		t.Fatal("int64 path parameter was not classified as a supported positional argument")
	}
}

func TestGeneratedIntegerParsersUseStrictParsing(t *testing.T) {
	src, err := renderMasterFile(nil)
	if err != nil {
		t.Fatal(err)
	}
	generated := string(src)
	for _, want := range []string{
		`strconv.Atoi(s)`,
		`strconv.ParseInt(s, 10, 64)`,
		`if n < 1`,
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated runtime does not contain %q", want)
		}
	}
	if strings.Contains(generated, `fmt.Sscanf`) {
		t.Fatal("generated runtime still accepts numeric prefixes with fmt.Sscanf")
	}
}
