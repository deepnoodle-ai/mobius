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

func TestNamedStringPathParamsAreCastToGeneratedType(t *testing.T) {
	var b bytes.Buffer
	err := renderCommand(&b, "resources", PlannedCommand{
		OperationID: "transitionResourceOwnership",
		Command:     "transition-ownership",
		Description: "Change resource ownership",
		Method: &Method{Params: []Param{
			{Name: "resourceType", Type: "TransitionResourceOwnershipParamsResourceType"},
		}},
		PathParams: []PathArg{{
			GoName: "resourceType", FlagName: "resource-type", GoType: "TransitionResourceOwnershipParamsResourceType",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `p0 := api.TransitionResourceOwnershipParamsResourceType(ctx.Arg(0))`
	if !strings.Contains(b.String(), want) {
		t.Fatalf("generated command does not contain %q:\n%s", want, b.String())
	}
}

func TestRequiredNamedStringQueryParamsBecomeRequiredFlags(t *testing.T) {
	client := &ClientInfo{TypeAliases: map[string]string{"AgentVisibility": "string"}}
	field := resolveQueryField(FieldInfo{
		GoName:  "Visibility",
		Type:    "AgentVisibility",
		JSONTag: "visibility",
		Doc:     "The visibility being considered.",
	}, client)
	if field.Kind != "string" || !field.Required || field.Description != "The visibility being considered." {
		t.Fatalf("query field = %#v, want required string", field)
	}

	var b bytes.Buffer
	err := renderCommand(&b, "agents", PlannedCommand{
		OperationID: "previewAgentVisibilityChange",
		Command:     "preview-visibility-change",
		Description: "Preview an agent visibility change",
		Method: &Method{
			Name: "PreviewAgentVisibilityChange",
			Params: []Param{{
				Name: "params",
				Type: "*PreviewAgentVisibilityChangeParams",
			}},
		},
		QueryBlock: &QueryBlock{
			TypeName: "PreviewAgentVisibilityChangeParams",
			Fields:   []QueryField{field},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	generated := b.String()
	for _, want := range []string{
		`cli.String("visibility", "").Help("[required] The visibility being considered.").Required()`,
		`params.Visibility = api.AgentVisibility(ctx.String("visibility"))`,
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated command does not contain %q:\n%s", want, generated)
		}
	}
}

func TestSensitiveDryRunFieldDetection(t *testing.T) {
	for _, name := range []string{"signing-secret", "api-key", "access-token", "values"} {
		if !isSensitiveFlagName(name) {
			t.Fatalf("%q should be redacted", name)
		}
	}
	if isSensitiveFlagName("description") {
		t.Fatal("ordinary descriptions should remain visible")
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

func TestGeneratedRuntimeHardensRequestInputs(t *testing.T) {
	src, err := renderMasterFile(nil)
	if err != nil {
		t.Fatal(err)
	}
	generated := string(src)
	for _, want := range []string{
		`dec.DisallowUnknownFields()`,
		`func decodeFlagText(`,
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated runtime does not contain %q", want)
		}
	}
}

func TestGeneratedCommandsOptIntoTextFiles(t *testing.T) {
	if !acceptsTextFileInput(BodyField{Kind: "string", ElemType: "string", FlagName: "instructions"}) {
		t.Fatal("instructions should accept @file text input")
	}
}
