package main

import (
	"bytes"
	"fmt"
	"go/format"
	"sort"
	"strings"
)

// Plan is the fully-resolved list of commands the generator will emit,
// together with the import set it needs.
type Plan struct {
	Commands []PlannedCommand
}

// PlannedCommand is one CLI command the generator is going to write.
type PlannedCommand struct {
	OperationID string
	Group       string // subcommand group (e.g. "workflows"); empty for root-level
	Command     string // leaf command name (e.g. "list")
	Description string

	Method     *Method     // source method on *ClientWithResponses
	PathParams []PathArg   // in signature order
	QueryBlock *QueryBlock // non-nil if method takes *{OpId}Params
	Body       *BodyArg    // non-nil if method takes a JSONRequestBody
}

// PathArg is a path parameter surfaced on the CLI. Most path params are
// positional arguments; some (e.g. "project") are surfaced as global flags
// on the root app, so the command reads them via ctx.String instead of
// consuming a positional slot.
type PathArg struct {
	GoName   string // signature argument name (e.g. "id")
	FlagName string // kebab-case CLI flag name (e.g. "id")
	GoType   string // e.g. "IDParam" or "string"
	IsInt    bool
	AsFlag   bool // read from a global flag rather than a positional arg
}

// QueryBlock captures the *{OpId}Params query-params struct.
type QueryBlock struct {
	TypeName string // e.g. "ListChannelsParams"
	Fields   []QueryField
}

// QueryField is one field of a query-params struct, surfaced as a CLI flag.
type QueryField struct {
	GoField     string // struct field name (e.g. "Kind")
	FlagName    string // kebab-case CLI flag name (e.g. "kind")
	Description string // help text from the OpenAPI parameter description
	ElemType    string // unwrapped element type: "string", "int", "bool", or a named alias
	Kind        string // "string", "int", "bool", "strings" (for []string), "skip"
}

// BodyArg captures a typed JSON request body.
type BodyArg struct {
	GoName   string      // signature argument name (e.g. "body")
	TypeName string      // e.g. "CreateChannelJSONRequestBody"
	Fields   []BodyField // per-field flags extracted from the underlying request struct
}

// BodyField is one field of a JSON request body, surfaced as a CLI flag.
// Required=true means the Go type is non-pointer (and so the server requires
// the field). When --file supplies the value, flags act as overrides.
type BodyField struct {
	GoField     string // struct field name (e.g. "Name")
	FlagName    string // kebab-case CLI flag name (e.g. "name")
	Description string // help text
	ElemType    string // unwrapped element type (e.g. "string", or a named alias)
	Kind        string // "string", "int", "int64", "bool", "strings", "skip"
	Required    bool   // true if the struct field is non-pointer
}

// buildPlan resolves every ClientWithResponses method against the OpenAPI spec
// and the override table, producing a deterministic list of commands.
func buildPlan(client *ClientInfo, spec map[string]*SpecOp, overrides map[string]Override) (*Plan, []string) {
	var (
		plan  Plan
		warns []string
	)

	// Sort method names for stable output.
	names := make([]string, 0, len(client.Methods))
	for n := range client.Methods {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		method := client.Methods[name]
		opID := lowerFirst(name)
		op := spec[opID]
		if op == nil {
			warns = append(warns, fmt.Sprintf("skip %s: no matching operationId in spec", name))
			continue
		}
		ov := overrides[opID]
		if ov.Skip {
			continue
		}

		pc := PlannedCommand{
			OperationID: opID,
			Method:      method,
			Group:       firstNonEmpty(ov.Group, groupFromSpec(op)),
			Command:     firstNonEmpty(ov.Command, commandLeaf(opID)),
			Description: firstNonEmpty(ov.Description, op.Summary),
		}
		if pc.Group == "" {
			warns = append(warns, fmt.Sprintf("skip %s: no tag or derivable group", name))
			continue
		}
		if pc.Description == "" {
			pc.Description = fmt.Sprintf("%s %s", strings.ToUpper(op.Method), op.Path)
		}

		ok, reason := classifyParams(method, client, &pc)
		if !ok {
			warns = append(warns, fmt.Sprintf("skip %s: %s", name, reason))
			continue
		}
		if pc.QueryBlock != nil && op != nil {
			for i := range pc.QueryBlock.Fields {
				f := &pc.QueryBlock.Fields[i]
				jsonName := strings.ReplaceAll(f.FlagName, "-", "_")
				if desc, ok := op.ParamDescriptions[jsonName]; ok {
					f.Description = desc
				}
			}
		}
		plan.Commands = append(plan.Commands, pc)
	}
	return &plan, warns
}

// classifyParams walks the method signature and fills in path/query/body on
// the PlannedCommand. Returns (false, reason) if the signature is too exotic
// for the generator to handle.
func classifyParams(m *Method, client *ClientInfo, pc *PlannedCommand) (bool, string) {
	for _, p := range m.Params {
		switch {
		case strings.HasPrefix(p.Type, "*") && strings.HasSuffix(p.Type, "Params"):
			// Query params struct.
			tname := strings.TrimPrefix(p.Type, "*")
			si := client.ParamStructs[tname]
			if si == nil {
				return false, fmt.Sprintf("unknown params struct %q", tname)
			}
			qb := &QueryBlock{TypeName: tname}
			for _, f := range si.Fields {
				qf := resolveQueryField(f, client)
				if qf.Kind == "skip" {
					continue
				}
				qb.Fields = append(qb.Fields, qf)
			}
			pc.QueryBlock = qb
		case strings.HasSuffix(p.Type, "JSONRequestBody"):
			ba := &BodyArg{GoName: p.Name, TypeName: p.Type}
			if si := client.BodyStructs[p.Type]; si != nil {
				for _, f := range si.Fields {
					ba.Fields = append(ba.Fields, resolveBodyField(f, client))
				}
			}
			pc.Body = ba
		case isSimplePathParam(p.Type, client):
			kind := underlyingKind(p.Type, client)
			arg := PathArg{
				GoName:   p.Name,
				FlagName: toKebab(p.Name),
				GoType:   p.Type,
				IsInt:    kind == "int",
			}
			if p.Name == "project" && kind == "string" {
				arg.AsFlag = true
			}
			pc.PathParams = append(pc.PathParams, arg)
		default:
			return false, fmt.Sprintf("unsupported parameter type %q", p.Type)
		}
	}
	return true, ""
}

// resolveBodyField maps a request-body struct field to a CLI flag descriptor.
// Unlike query params, body fields may be non-pointer (required). Composite
// types — named structs, slices, maps — are surfaced as cli.String flags that
// accept a JSON literal and are unmarshaled at runtime.
func resolveBodyField(f FieldInfo, client *ClientInfo) BodyField {
	bf := BodyField{
		GoField:     f.GoName,
		FlagName:    toKebab(firstNonEmpty(f.JSONTag, f.GoName)),
		Description: f.Doc,
	}
	elem := f.Type
	if strings.HasPrefix(elem, "*") {
		elem = strings.TrimPrefix(elem, "*")
	} else if !f.Omit {
		// Non-pointer + no omitempty means the field is required on the wire.
		// A non-pointer tagged omitempty is still optional (oapi-codegen emits
		// this for a handful of map/interface fields).
		bf.Required = true
	}
	// Slices: []string (optionally via alias) maps to cli.Strings; anything
	// else becomes a JSON-string flag.
	if strings.HasPrefix(elem, "[]") {
		inner := strings.TrimPrefix(elem, "[]")
		if underlyingKind(inner, client) == "string" {
			bf.ElemType = elem
			bf.Kind = "strings"
			return bf
		}
		bf.Kind = "json"
		return bf
	}
	bf.ElemType = elem
	switch underlyingKind(elem, client) {
	case "string":
		bf.Kind = "string"
	case "int":
		bf.Kind = "int"
	case "int64":
		bf.Kind = "int64"
	case "bool":
		bf.Kind = "bool"
	default:
		bf.Kind = "json"
	}
	return bf
}

// resolveQueryField maps a struct field to a CLI flag descriptor.
func resolveQueryField(f FieldInfo, client *ClientInfo) QueryField {
	qf := QueryField{
		GoField:  f.GoName,
		FlagName: toKebab(firstNonEmpty(f.JSONTag, f.GoName)),
	}
	// Expect pointer types for query fields; anything else we skip.
	if !strings.HasPrefix(f.Type, "*") {
		qf.Kind = "skip"
		return qf
	}
	elem := strings.TrimPrefix(f.Type, "*")
	// []string → cli.Strings
	if strings.HasPrefix(elem, "[]") {
		inner := strings.TrimPrefix(elem, "[]")
		if underlyingKind(inner, client) == "string" {
			qf.ElemType = elem
			qf.Kind = "strings"
			return qf
		}
		qf.Kind = "skip"
		return qf
	}
	qf.ElemType = elem
	switch underlyingKind(elem, client) {
	case "string":
		qf.Kind = "string"
	case "int":
		qf.Kind = "int"
	case "int64":
		qf.Kind = "int64"
	case "bool":
		qf.Kind = "bool"
	default:
		qf.Kind = "skip"
	}
	return qf
}

// underlyingKind returns "string", "int", "bool", or "" for a named type,
// following type-alias chains recorded in client.TypeAliases.
func underlyingKind(t string, client *ClientInfo) string {
	seen := map[string]bool{}
	for !seen[t] {
		seen[t] = true
		switch t {
		case "string", "int", "int64", "bool":
			return t
		}
		next, ok := client.TypeAliases[t]
		if !ok {
			return ""
		}
		t = next
	}
	return ""
}

func isBuiltin(t string) bool {
	switch t {
	case "string", "int", "int64", "bool":
		return true
	}
	return false
}

// summarizeHelp turns a verbose OpenAPI description into a one-line CLI
// help string. It takes the first paragraph (up to a blank line, the
// schema-doc convention for "short summary"), collapses whitespace, and
// hard-caps the result so wonton's non-wrapping help renderer doesn't
// truncate it at the terminal edge.
func summarizeHelp(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if idx := strings.Index(s, "\n\n"); idx > 0 {
		s = s[:idx]
	}
	s = strings.Join(strings.Fields(s), " ")
	const maxHelpLen = 100
	if len(s) > maxHelpLen {
		s = s[:maxHelpLen-1] + "…"
	}
	return s
}

// isReservedFlag reports whether a body-field flag name would collide with a
// global flag or the body file flag. Such fields stay file-only.
func isReservedFlag(name string) bool {
	switch name {
	case "api-url", "api-key", "project", "profile", "log-level",
		"output", "fields", "quiet", "var", "file", "dry-run":
		return true
	}
	return false
}

// isTagMapField reports whether a body field is the conventional Azure-style
// tag map. Such fields get a friendlier --tag KEY=VALUE repeatable surface
// instead of the default --tags <JSON> flag.
func isTagMapField(f BodyField) bool {
	if f.FlagName != "tags" {
		return false
	}
	t := strings.TrimPrefix(f.ElemType, "[]")
	t = strings.TrimPrefix(t, "*")
	return t == "TagMap"
}

func isSimplePathParam(t string, client *ClientInfo) bool {
	switch underlyingKind(t, client) {
	case "string", "int":
		return true
	}
	return false
}

// groupFromSpec derives a subcommand group from an operation's first tag, or
// from the first path segment as a fallback.
func groupFromSpec(op *SpecOp) string {
	if op.Tag != "" {
		return toKebab(op.Tag)
	}
	parts := strings.Split(strings.Trim(op.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	return toKebab(parts[0])
}

// commandLeaf turns an operationId into a short command name by stripping the
// verb-like prefix (list/get/create/update/delete) and everything after it.
// Example: "listWorkflows" -> "list", "getChannelMessage" -> "get-message"
// (we keep any noun suffix past the resource name so sibling operations on
// the same resource don't collide).
func commandLeaf(opID string) string {
	verbs := []string{
		"list", "get", "create", "update", "delete", "revoke", "rotate",
		"cancel", "retry", "bulk", "add", "remove", "send", "claim",
		"disconnect", "handle", "heartbeat",
	}
	lo := opID
	for _, v := range verbs {
		if strings.HasPrefix(lo, v) && len(lo) > len(v) {
			rest := lo[len(v):]
			// Strip the immediate resource token (first PascalCase word).
			trimmed := stripLeadingWord(rest)
			if trimmed == "" {
				return v
			}
			return v + "-" + toKebab(lowerFirst(trimmed))
		}
	}
	return toKebab(opID)
}

// stripLeadingWord removes the first PascalCase word from s. It assumes s
// begins with an uppercase letter (because it comes after a lowercase verb).
//
// Initialism runs (e.g. "API", "URL") are treated as a single word, so
// stripLeadingWord("APIKey") returns "Key" rather than "PIKey".
func stripLeadingWord(s string) string {
	if s == "" {
		return ""
	}
	// Initialism run: two or more consecutive uppercase letters at the start.
	// Consume the run; if a lowercase letter follows, the last uppercase in
	// the run is actually the start of the *next* word (e.g. "APIKey" → the
	// "K" starts "Key"), so rewind one char.
	if len(s) >= 2 && isUpper(s[0]) && isUpper(s[1]) {
		i := 2
		for i < len(s) && isUpper(s[i]) {
			i++
		}
		if i == len(s) {
			return ""
		}
		return s[i-1:]
	}
	// Single-capital PascalCase word: strip until the next uppercase.
	for i := 1; i < len(s); i++ {
		if isUpper(s[i]) {
			return s[i:]
		}
	}
	return ""
}

// render emits one source file per command group, plus a master
// commands.gen.go that dispatches to them and defines shared helpers.
//
// The returned map is keyed by file basename (e.g. "commands.gen.go",
// "commands_workflows.gen.go") so callers can write them into the same
// output directory.
func render(plan *Plan, groupDescriptions map[string]string) (map[string][]byte, error) {
	// Group commands by their parent group and dedupe leaf names within each.
	groups := map[string][]PlannedCommand{}
	var groupOrder []string
	for _, c := range plan.Commands {
		if _, seen := groups[c.Group]; !seen {
			groupOrder = append(groupOrder, c.Group)
		}
		groups[c.Group] = append(groups[c.Group], c)
	}
	sort.Strings(groupOrder)
	for _, g := range groupOrder {
		cmds := groups[g]
		sort.SliceStable(cmds, func(i, j int) bool { return cmds[i].Command < cmds[j].Command })
		used := map[string]int{}
		for i := range cmds {
			leaf := cmds[i].Command
			if n := used[leaf]; n > 0 {
				cmds[i].Command = fmt.Sprintf("%s-%d", leaf, n+1)
			}
			used[leaf]++
		}
		groups[g] = cmds
	}

	files := map[string][]byte{}

	// One file per group.
	for _, g := range groupOrder {
		src, err := renderGroupFile(g, groups[g], groupDescriptions[g])
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", g, err)
		}
		files[groupFileName(g)] = src
	}

	// Master file: dispatcher + helpers.
	master, err := renderMasterFile(groupOrder)
	if err != nil {
		return nil, fmt.Errorf("master: %w", err)
	}
	files["commands.gen.go"] = master

	return files, nil
}

// groupFileName returns the basename for a group's generated file. Dashes in
// group names are converted to underscores so the file names are valid Go
// source identifiers for tooling that assumes snake_case.
func groupFileName(group string) string {
	return "commands_" + strings.ReplaceAll(group, "-", "_") + ".gen.go"
}

// singularAlias returns the singular form of a group name when the group name
// looks like a regular English plural (ending in "s"). It returns "" when no
// sensible singular alias exists — e.g. for "slack", or group names too short
// to strip a character from.
func singularAlias(group string) string {
	if len(group) < 2 || !strings.HasSuffix(group, "s") {
		return ""
	}
	return group[:len(group)-1]
}

// groupRegisterFunc returns the registrar function name for a group.
func groupRegisterFunc(group string) string {
	// e.g. "api-keys" -> "registerApiKeysCommands"
	parts := strings.Split(group, "-")
	out := "register"
	for _, p := range parts {
		if p == "" {
			continue
		}
		out += strings.ToUpper(p[:1]) + p[1:]
	}
	return out + "Commands"
}

// renderGroupFile writes the file containing every command for one group.
func renderGroupFile(group string, cmds []PlannedCommand, description string) ([]byte, error) {
	// Render commands first so we can inspect whether the `api` package is
	// actually used — some groups (e.g. workers, slack) only have no-arg
	// commands and would otherwise import it unused.
	var body bytes.Buffer
	if description != "" {
		fmt.Fprintf(&body, "\t%s := app.Group(%q).Description(%q)\n",
			groupVar(group), group, description)
	} else {
		fmt.Fprintf(&body, "\t%s := app.Group(%q)\n", groupVar(group), group)
	}
	if alias := singularAlias(group); alias != "" {
		fmt.Fprintf(&body, "\t%s.Alias(%q)\n", groupVar(group), alias)
	}
	for _, c := range cmds {
		if err := renderCommand(&body, group, c); err != nil {
			return nil, err
		}
	}
	usesAPI := bytes.Contains(body.Bytes(), []byte("api."))
	usesFmt := bytes.Contains(body.Bytes(), []byte("fmt."))
	usesJSON := bytes.Contains(body.Bytes(), []byte("json."))

	var b bytes.Buffer
	fmt.Fprintf(&b, `// Code generated by mobius-cligen. DO NOT EDIT.
//
// Regenerate with:  make generate-go-cli
//
// To suppress or override a command, edit
// cmd/mobius-cligen/overrides.go — never hand-edit this file.

package main

import (
`)
	stdlib := []string{}
	if usesJSON {
		stdlib = append(stdlib, "encoding/json")
	}
	if usesFmt {
		stdlib = append(stdlib, "fmt")
	}
	for _, p := range stdlib {
		fmt.Fprintf(&b, "\t%q\n", p)
	}
	if len(stdlib) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("\t\"github.com/deepnoodle-ai/wonton/cli\"\n")
	if usesAPI {
		b.WriteString("\n\t\"github.com/deepnoodle-ai/mobius/mobius/api\"\n")
	}
	fmt.Fprintf(&b, `)

// %s registers every generated subcommand in the %q group.
func %s(app *cli.App) {
`, groupRegisterFunc(group), group, groupRegisterFunc(group))
	b.Write(body.Bytes())
	b.WriteString("}\n")

	src, err := format.Source(b.Bytes())
	if err != nil {
		return b.Bytes(), fmt.Errorf("format: %w", err)
	}
	return src, nil
}

// renderMasterFile writes commands.gen.go with the dispatcher and shared
// runtime helpers used by every group file.
func renderMasterFile(groupOrder []string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`// Code generated by mobius-cligen. DO NOT EDIT.
//
// Regenerate with:  make generate-go-cli
//
// This file is the dispatcher: it calls the registrar in each
// commands_<group>.gen.go sibling. Shared runtime helpers used by the
// generated commands also live here so each group file can stay minimal.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"
	"gopkg.in/yaml.v3"
)

// registerGeneratedCommands attaches every generated subcommand to app by
// delegating to the per-group registrars in the commands_<group>.gen.go files.
func registerGeneratedCommands(app *cli.App) {
`)
	for _, g := range groupOrder {
		fmt.Fprintf(&b, "\t%s(app)\n", groupRegisterFunc(g))
	}
	b.WriteString("}\n\n")
	renderHelpers(&b)

	src, err := format.Source(b.Bytes())
	if err != nil {
		return b.Bytes(), fmt.Errorf("format: %w", err)
	}
	return src, nil
}

func renderCommand(b *bytes.Buffer, group string, c PlannedCommand) error {
	fmt.Fprintf(b, "\t%s.Command(%q).\n", groupVar(group), c.Command)
	fmt.Fprintf(b, "\t\tDescription(%q).\n", c.Description)

	// Positional args (path params that aren't surfaced as flags).
	var positional []string
	for _, p := range c.PathParams {
		if p.AsFlag {
			continue
		}
		positional = append(positional, fmt.Sprintf("%q", p.FlagName))
	}
	if len(positional) > 0 {
		fmt.Fprintf(b, "\t\tArgs(%s).\n", strings.Join(positional, ", "))
	}

	// Flags (query + body file). Flag-backed path params like --project are
	// declared as global flags on the root app, so we don't emit a
	// per-command flag for them here — the handler reads them via ctx.String.
	var flags []string
	if c.QueryBlock != nil {
		for _, f := range c.QueryBlock.Fields {
			help := summarizeHelp(firstNonEmpty(f.Description, f.FlagName))
			switch f.Kind {
			case "string":
				flags = append(flags, fmt.Sprintf(`cli.String(%q, "").Help(%q)`, f.FlagName, help))
			case "int", "int64":
				flags = append(flags, fmt.Sprintf(`cli.Int(%q, "").Help(%q)`, f.FlagName, help))
			case "bool":
				flags = append(flags, fmt.Sprintf(`cli.Bool(%q, "").Help(%q)`, f.FlagName, help))
			case "strings":
				flags = append(flags, fmt.Sprintf(`cli.Strings(%q, "").Help(%q)`, f.FlagName, help))
			}
		}
	}
	if c.Body != nil {
		for _, f := range c.Body.Fields {
			if isReservedFlag(f.FlagName) {
				continue
			}
			help := summarizeHelp(firstNonEmpty(f.Description, f.FlagName))
			if f.Required {
				help = "[required] " + help
			}
			if isTagMapField(f) {
				flags = append(flags, fmt.Sprintf(`cli.Strings(%q, "").Help(%q)`, "tag",
					"Tag in KEY=VALUE form. Repeatable."))
				continue
			}
			switch f.Kind {
			case "string":
				flags = append(flags, fmt.Sprintf(`cli.String(%q, "").Help(%q)`, f.FlagName, help))
			case "int", "int64":
				flags = append(flags, fmt.Sprintf(`cli.Int(%q, "").Help(%q)`, f.FlagName, help))
			case "bool":
				flags = append(flags, fmt.Sprintf(`cli.Bool(%q, "").Help(%q)`, f.FlagName, help))
			case "strings":
				flags = append(flags, fmt.Sprintf(`cli.Strings(%q, "").Help(%q)`, f.FlagName, help))
			case "json":
				// Suffix is short so we don't blow the line width.
				flags = append(flags, fmt.Sprintf(`cli.String(%q, "").Help(%q)`, f.FlagName,
					help+" Accepts JSON, @file, or @-."))
			}
		}
		flags = append(flags, `cli.String("file", "f").Help("Request body from a file (JSON or YAML, '-' for stdin). Flags override file contents.")`)
		flags = append(flags, `cli.Bool("dry-run", "").Help("Print the assembled request body and exit without sending it.")`)
	}
	if len(flags) > 0 {
		fmt.Fprintf(b, "\t\tFlags(\n")
		for _, f := range flags {
			fmt.Fprintf(b, "\t\t\t%s,\n", f)
		}
		fmt.Fprintf(b, "\t\t).\n")
	}

	fmt.Fprintf(b, "\t\tUse(requireAuth()).\n")
	fmt.Fprintf(b, "\t\tRun(func(ctx *cli.Context) error {\n")
	fmt.Fprintf(b, "\t\t\tmc, err := clientFromContext(ctx)\n")
	fmt.Fprintf(b, "\t\t\tif err != nil { return err }\n")
	fmt.Fprintf(b, "\t\t\tclient := mc.RawClient()\n")

	// Collect path-param locals. Positional path params consume args in
	// order; flag-backed ones (e.g. --project) read from their flag — except
	// "project" itself, which we read through authFor so a saved profile's
	// project handle still flows through when --project / MOBIUS_PROJECT
	// aren't explicitly set.
	argIdx := 0
	for i, p := range c.PathParams {
		switch {
		case p.AsFlag && p.FlagName == "project":
			fmt.Fprintf(b, "\t\t\tp%d := authFor(ctx).Project\n", i)
		case p.AsFlag:
			fmt.Fprintf(b, "\t\t\tp%d := ctx.String(%q)\n", i, p.FlagName)
		case p.IsInt:
			fmt.Fprintf(b, "\t\t\tp%d, err := parseIntArg(ctx.Arg(%d), %q)\n", i, argIdx, p.FlagName)
			fmt.Fprintf(b, "\t\t\tif err != nil { return err }\n")
			argIdx++
		default:
			fmt.Fprintf(b, "\t\t\tp%d := ctx.Arg(%d)\n", i, argIdx)
			argIdx++
		}
	}

	// Build *api.{QueryStruct} if needed.
	if c.QueryBlock != nil {
		fmt.Fprintf(b, "\t\t\tparams := &api.%s{}\n", c.QueryBlock.TypeName)
		for _, f := range c.QueryBlock.Fields {
			cast := func(goExpr string) string {
				if isBuiltin(f.ElemType) {
					return goExpr
				}
				return fmt.Sprintf("api.%s(%s)", f.ElemType, goExpr)
			}
			switch f.Kind {
			case "string":
				fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := %s; params.%s = &v }\n",
					f.FlagName, cast(fmt.Sprintf("ctx.String(%q)", f.FlagName)), f.GoField)
			case "int":
				fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := %s; params.%s = &v }\n",
					f.FlagName, cast(fmt.Sprintf("ctx.Int(%q)", f.FlagName)), f.GoField)
			case "int64":
				fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := %s; params.%s = &v }\n",
					f.FlagName, cast(fmt.Sprintf("int64(ctx.Int(%q))", f.FlagName)), f.GoField)
			case "bool":
				fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := ctx.Bool(%q); params.%s = &v }\n",
					f.FlagName, f.FlagName, f.GoField)
			case "strings":
				fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := ctx.Strings(%q); params.%s = &v }\n",
					f.FlagName, f.FlagName, f.GoField)
			}
		}
	}

	// Body decoding.
	if c.Body != nil {
		fmt.Fprintf(b, "\t\t\tvar body api.%s\n", c.Body.TypeName)
		fmt.Fprintf(b, "\t\t\tif err := readJSONBody(ctx, &body); err != nil { return err }\n")
		// Per-field flag overrides. Flags beat file contents.
		for _, f := range c.Body.Fields {
			if isReservedFlag(f.FlagName) {
				continue
			}
			if isTagMapField(f) {
				fmt.Fprintf(b, "\t\t\tif tags, err := parseTagFlags(ctx); err != nil { return err } else if tags != nil { v := api.TagMap(tags); body.%s = &v }\n",
					f.GoField)
				continue
			}
			cast := func(goExpr string) string {
				if isBuiltin(f.ElemType) {
					return goExpr
				}
				return fmt.Sprintf("api.%s(%s)", f.ElemType, goExpr)
			}
			switch f.Kind {
			case "string":
				if f.Required {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { body.%s = %s }\n",
						f.FlagName, f.GoField, cast(fmt.Sprintf("ctx.String(%q)", f.FlagName)))
				} else {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := %s; body.%s = &v }\n",
						f.FlagName, cast(fmt.Sprintf("ctx.String(%q)", f.FlagName)), f.GoField)
				}
			case "int":
				if f.Required {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { body.%s = %s }\n",
						f.FlagName, f.GoField, cast(fmt.Sprintf("ctx.Int(%q)", f.FlagName)))
				} else {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := %s; body.%s = &v }\n",
						f.FlagName, cast(fmt.Sprintf("ctx.Int(%q)", f.FlagName)), f.GoField)
				}
			case "int64":
				if f.Required {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { body.%s = %s }\n",
						f.FlagName, f.GoField, cast(fmt.Sprintf("int64(ctx.Int(%q))", f.FlagName)))
				} else {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := %s; body.%s = &v }\n",
						f.FlagName, cast(fmt.Sprintf("int64(ctx.Int(%q))", f.FlagName)), f.GoField)
				}
			case "bool":
				if f.Required {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { body.%s = ctx.Bool(%q) }\n",
						f.FlagName, f.GoField, f.FlagName)
				} else {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := ctx.Bool(%q); body.%s = &v }\n",
						f.FlagName, f.FlagName, f.GoField)
				}
			case "strings":
				if f.Required {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { body.%s = ctx.Strings(%q) }\n",
						f.FlagName, f.GoField, f.FlagName)
				} else {
					fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { v := ctx.Strings(%q); body.%s = &v }\n",
						f.FlagName, f.FlagName, f.GoField)
				}
			case "json":
				fmt.Fprintf(b, "\t\t\tif ctx.IsSet(%q) { if err := decodeFlagJSON(ctx, %q, ctx.String(%q), &body.%s); err != nil { return err } }\n",
					f.FlagName, f.FlagName, f.FlagName, f.GoField)
			}
		}
		// Client-side required-field check. If neither --file nor the flag set
		// the value, surface a friendly error rather than letting the server
		// reject the request.
		for _, f := range c.Body.Fields {
			if !f.Required || isReservedFlag(f.FlagName) {
				continue
			}
			var zeroExpr string
			switch f.Kind {
			case "string":
				zeroExpr = fmt.Sprintf("body.%s == \"\"", f.GoField)
			case "int", "int64":
				zeroExpr = fmt.Sprintf("body.%s == 0", f.GoField)
			case "strings":
				zeroExpr = fmt.Sprintf("len(body.%s) == 0", f.GoField)
			case "json":
				// Can't reliably zero-check an arbitrary Go value; fall back
				// to "was either --file or the flag provided".
				zeroExpr = fmt.Sprintf("ctx.String(\"file\") == \"\" && !ctx.IsSet(%q)", f.FlagName)
			default:
				continue
			}
			fmt.Fprintf(b, "\t\t\tif %s { return fmt.Errorf(\"--%s is required (or supply it via --file)\") }\n",
				zeroExpr, f.FlagName)
		}
		// No-required-field commands (typically update/patch) should reject a
		// call with no flags and no --file so a bare invocation doesn't
		// silently no-op.
		hasRequired := false
		for _, f := range c.Body.Fields {
			if f.Required && !isReservedFlag(f.FlagName) {
				hasRequired = true
				break
			}
		}
		if !hasRequired {
			conds := []string{`ctx.String("file") == ""`}
			for _, f := range c.Body.Fields {
				if f.Kind == "skip" || isReservedFlag(f.FlagName) {
					continue
				}
				flagName := f.FlagName
				if isTagMapField(f) {
					flagName = "tag"
				}
				conds = append(conds, fmt.Sprintf("!ctx.IsSet(%q)", flagName))
			}
			fmt.Fprintf(b, "\t\t\tif %s { return fmt.Errorf(\"at least one flag or --file is required\") }\n",
				strings.Join(conds, " && "))
		}
		// --dry-run: print the assembled body and exit before HTTP.
		fmt.Fprintf(b, "\t\t\tif ctx.Bool(\"dry-run\") { return printDryRun(ctx, body) }\n")
	}

	// Build the call argument list, matching method signature order.
	var args []string
	args = append(args, "ctx.Context()")
	pathIdx := 0
	for _, p := range c.Method.Params {
		switch {
		case strings.HasPrefix(p.Type, "*") && strings.HasSuffix(p.Type, "Params"):
			args = append(args, "params")
		case strings.HasSuffix(p.Type, "JSONRequestBody"):
			args = append(args, "body")
		default:
			args = append(args, fmt.Sprintf("p%d", pathIdx))
			pathIdx++
		}
	}
	fmt.Fprintf(b, "\t\t\tresp, err := client.%sWithResponse(%s)\n", c.Method.Name, strings.Join(args, ", "))
	fmt.Fprintf(b, "\t\t\tif err != nil { return err }\n")
	fmt.Fprintf(b, "\t\t\treturn printResponse(ctx, %q, resp.StatusCode(), resp.Body)\n", c.OperationID)
	fmt.Fprintf(b, "\t\t})\n\n")
	return nil
}

// renderHelpers writes the runtime helpers the generated commands rely on.
func renderHelpers(b *bytes.Buffer) {
	b.WriteString(`// readJSONBody reads a request body from --file (path or '-' for stdin) and
// unmarshals it into v. JSON and YAML are both accepted; the format is
// detected from the file extension (.yaml/.yml/.json) or, for stdin and
// unknown extensions, by sniffing (JSON first, then YAML). When --file is not
// set this is a no-op so per-field flags alone can populate the body.
//
// --var KEY=VALUE substitutions (when present) are applied to the file
// contents before parsing.
func readJSONBody(ctx *cli.Context, v any) error {
	path := ctx.String("file")
	if path == "" {
		return nil
	}
	data, label, err := readBodyBytes(ctx, path)
	if err != nil {
		return err
	}
	data, err = applyVars(ctx, data)
	if err != nil {
		return err
	}
	return decodeJSONOrYAML(data, label, path, v)
}

// decodeFlagJSON decodes a JSON-typed flag value. The value can be:
//   - bare JSON literal (e.g. {"k":"v"})
//   - "@path" to read from a file (JSON or YAML)
//   - "@-" to read from stdin
//
// --var substitutions are applied to file contents before parse.
func decodeFlagJSON(ctx *cli.Context, flag, raw string, v any) error {
	if strings.HasPrefix(raw, "@") {
		path := raw[1:]
		if path == "" {
			return cli.Errorf("--%s: expected @<path> or @-, got bare @", flag)
		}
		data, label, err := readBodyBytes(ctx, path)
		if err != nil {
			return cli.Errorf("--%s: %v", flag, err)
		}
		data, err = applyVars(ctx, data)
		if err != nil {
			return cli.Errorf("--%s: %v", flag, err)
		}
		if err := decodeJSONOrYAML(data, label, path, v); err != nil {
			return cli.Errorf("--%s: %v", flag, err)
		}
		return nil
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return cli.Errorf("--%s: invalid JSON: %v", flag, jsonErrLocation(err, []byte(raw)))
	}
	return nil
}

// readBodyBytes reads from a path or "-" (stdin). The returned label is
// "json" or "yaml" when known from the extension, otherwise empty.
func readBodyBytes(ctx *cli.Context, path string) ([]byte, string, error) {
	if path == "-" {
		data, err := io.ReadAll(ctx.Stdin())
		if err != nil {
			return nil, "", fmt.Errorf("read stdin: %w", err)
		}
		return data, "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return data, "yaml", nil
	case ".json":
		return data, "json", nil
	}
	return data, "", nil
}

// decodeJSONOrYAML unmarshals data into v, choosing the parser by label
// (from the file extension) or by sniffing. YAML is round-tripped through
// JSON so target types containing oneOf/union json.RawMessage fields (which
// have no yaml tags) still hydrate correctly.
func decodeJSONOrYAML(data []byte, label, path string, v any) error {
	disp := displayPath(path)
	switch label {
	case "json":
		if err := json.Unmarshal(data, v); err != nil {
			return fmt.Errorf("parse %s: %w", disp, jsonErrLocation(err, data))
		}
		return nil
	case "yaml":
		return decodeYAMLViaJSON(data, disp, v)
	}
	if looksLikeJSON(data) {
		if err := json.Unmarshal(data, v); err != nil {
			return fmt.Errorf("parse %s: %w", disp, jsonErrLocation(err, data))
		}
		return nil
	}
	return decodeYAMLViaJSON(data, disp, v)
}

// decodeYAMLViaJSON parses YAML into a generic structure, re-marshals it as
// JSON, and unmarshals into v. Routing through JSON makes target types whose
// oneOf/union fields carry json tags (but no yaml tags) work as expected.
func decodeYAMLViaJSON(data []byte, disp string, v any) error {
	var generic any
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return fmt.Errorf("parse %s: %w", disp, err)
	}
	generic = normalizeYAMLForJSON(generic)
	jsonBytes, err := json.Marshal(generic)
	if err != nil {
		return fmt.Errorf("convert %s yaml to json: %w", disp, err)
	}
	if err := json.Unmarshal(jsonBytes, v); err != nil {
		return fmt.Errorf("parse %s: %w", disp, err)
	}
	return nil
}

// normalizeYAMLForJSON converts map[interface{}]interface{} (produced by some
// yaml.v3 paths for nested maps) into map[string]interface{} so json.Marshal
// can encode them.
func normalizeYAMLForJSON(v any) any {
	switch x := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[fmt.Sprint(k)] = normalizeYAMLForJSON(vv)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = normalizeYAMLForJSON(vv)
		}
		return out
	case []any:
		for i := range x {
			x[i] = normalizeYAMLForJSON(x[i])
		}
		return x
	default:
		return v
	}
}

// jsonErrLocation enriches a json error with a line:col when possible.
func jsonErrLocation(err error, data []byte) error {
	if err == nil {
		return nil
	}
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		line, col := offsetToLineCol(data, int(syn.Offset))
		return fmt.Errorf("%s (line %d, col %d)", syn.Error(), line, col)
	}
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		line, col := offsetToLineCol(data, int(ute.Offset))
		return fmt.Errorf("%s (line %d, col %d, field %q)", ute.Error(), line, col, ute.Field)
	}
	return err
}

func offsetToLineCol(data []byte, off int) (int, int) {
	if off > len(data) {
		off = len(data)
	}
	line, col := 1, 1
	for i := 0; i < off; i++ {
		if data[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

func looksLikeJSON(data []byte) bool {
	for _, c := range data {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

func displayPath(p string) string {
	if p == "-" || p == "" {
		return "<stdin>"
	}
	return p
}

// applyVars performs ${KEY} substitution on data using --var KEY=VALUE pairs.
// Unknown keys are left intact (so workflow specs with intentional ${...}
// placeholders pass through untouched). Returns data unchanged when no --var
// flag was set.
func applyVars(ctx *cli.Context, data []byte) ([]byte, error) {
	if !ctx.IsSet("var") {
		return data, nil
	}
	pairs := ctx.Strings("var")
	if len(pairs) == 0 {
		return data, nil
	}
	vars := make(map[string]string, len(pairs))
	for _, p := range pairs {
		eq := strings.IndexByte(p, '=')
		if eq <= 0 {
			return nil, cli.Errorf("--var %q: expected KEY=VALUE", p)
		}
		vars[p[:eq]] = p[eq+1:]
	}
	out := os.Expand(string(data), func(k string) string {
		if v, ok := vars[k]; ok {
			return v
		}
		return "${" + k + "}"
	})
	return []byte(out), nil
}

// printDryRun pretty-prints a request body without sending HTTP. Generated
// handlers call this when --dry-run is set, then return its result so the
// command exits 0.
func printDryRun(ctx *cli.Context, body any) error {
	pretty, err := marshalForOutput(ctx, body)
	if err != nil {
		return err
	}
	ctx.Println(string(pretty))
	return nil
}

// parseTagFlags converts repeatable --tag KEY=VALUE flags into a map. Returns
// nil when no --tag is set so callers can leave the field unset.
func parseTagFlags(ctx *cli.Context) (map[string]string, error) {
	if !ctx.IsSet("tag") {
		return nil, nil
	}
	pairs := ctx.Strings("tag")
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		eq := strings.IndexByte(p, '=')
		if eq <= 0 {
			return nil, cli.Errorf("--tag %q: expected KEY=VALUE", p)
		}
		out[p[:eq]] = p[eq+1:]
	}
	return out, nil
}

// printResponse renders the response body according to the --output (-o)
// global flag. The operationId lets us route to a custom per-command pretty
// renderer (registered via RegisterResponseRenderer in a hand-written
// sibling) when the resolved mode is "pretty"; machine-parseable modes
// (json, yaml, id, name) always go through the generic path so scripts get a
// stable shape. Non-2xx responses are surfaced as errors with a typed exit
// code so callers exit with a meaningful status.
func printResponse(ctx *cli.Context, opID string, status int, body []byte) error {
	if status >= 200 && status < 300 {
		if ctx.Bool("quiet") {
			return nil
		}
		return renderResponse(ctx, opID, body)
	}
	pretty := body
	if looksLikeJSON(body) {
		var tmp any
		if err := json.Unmarshal(body, &tmp); err == nil {
			if buf, err := json.MarshalIndent(tmp, "", "  "); err == nil {
				pretty = buf
			}
		}
	}
	return &cli.ExitError{Code: exitCodeForStatus(status), Message: fmt.Sprintf("HTTP %d: %s", status, string(pretty))}
}

func exitCodeForStatus(status int) int {
	switch {
	case status >= 500:
		return 4
	case status >= 400:
		return 5
	default:
		return 1
	}
}

// renderResponse writes the response body to ctx.Stdout() per --output mode.
//
// --fields, when set, projects each row down to the requested keys before
// formatting. Unknown fields are rejected with the available field list so
// scripts fail loudly on typos. In pretty mode (auto on a TTY, or explicit
// --output pretty) a registered per-operation renderer takes precedence
// over the generic shape-driven renderer — but only when --fields is unset,
// because custom renderers don't know about field projection.
func renderResponse(ctx *cli.Context, opID string, body []byte) error {
	if len(body) == 0 {
		return nil
	}
	format := strings.ToLower(ctx.String("output"))
	if format == "" || format == "auto" {
		if stdoutIsTerminal(ctx) {
			format = "pretty"
		} else {
			format = "json"
		}
	}

	fields := parseFieldsFlag(ctx)

	// Custom renderers only fire when no projection is requested — they own
	// the full shape and would lose context if we pre-projected.
	if format == "pretty" && len(fields) == 0 {
		if fn := responseRendererFor(opID); fn != nil {
			return fn(ctx, body)
		}
	}

	if !looksLikeJSON(body) {
		// Non-JSON body: print as-is, ignore --fields.
		ctx.Println(string(body))
		return nil
	}

	if len(fields) > 0 {
		// Validate up front so a bad field errors before we render
		// anything. Pretty rendering is a non-data formatter and won't
		// surface the typo otherwise.
		if err := validateFieldsInBody(body, fields); err != nil {
			return err
		}
		if format == "pretty" {
			return renderPretty(ctx, body, fields...)
		}
		projected, err := projectBody(body, fields)
		if err != nil {
			return err
		}
		return writeFormat(ctx, format, projected)
	}
	return writeFormatRaw(ctx, format, body)
}

// validateFieldsInBody rejects unknown field names against the first row of
// a list response (or the resource itself for a single object). This runs
// before rendering so typos fail loudly regardless of the chosen format.
func validateFieldsInBody(body []byte, fields []string) error {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if items, isList := obj["items"].([]any); isList {
		if len(items) == 0 {
			return nil
		}
		first, _ := items[0].(map[string]any)
		return validateFields(first, fields)
	}
	return validateFields(obj, fields)
}

// parseFieldsFlag returns the requested projection. wonton's Strings flag is
// already repeatable (--fields a --fields b); we additionally split each
// value on "," so --fields a,b works too.
func parseFieldsFlag(ctx *cli.Context) []string {
	if !ctx.IsSet("fields") {
		return nil
	}
	var out []string
	for _, raw := range ctx.Strings("fields") {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// projectBody returns a JSON-serializable value containing only the
// requested fields. For a paginated list envelope we keep the envelope keys
// (has_more, next_cursor, …) and project items[]; for a single resource we
// project the resource itself.
//
// Returns an error naming the unknown field and listing what is available
// so callers see actionable feedback on a typo.
func projectBody(body []byte, fields []string) (any, error) {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return raw, nil
	}
	if items, isList := obj["items"].([]any); isList {
		if len(items) > 0 {
			first, _ := items[0].(map[string]any)
			if err := validateFields(first, fields); err != nil {
				return nil, err
			}
		}
		projected := make([]any, len(items))
		for i, it := range items {
			row, _ := it.(map[string]any)
			projected[i] = pickFields(row, fields)
		}
		out := make(map[string]any, len(obj))
		for k, v := range obj {
			out[k] = v
		}
		out["items"] = projected
		return out, nil
	}
	if err := validateFields(obj, fields); err != nil {
		return nil, err
	}
	return pickFields(obj, fields), nil
}

// validateFields errors when any requested field is missing from sample.
// The error includes the sorted list of available fields so the user can
// fix a typo in one shot.
func validateFields(sample map[string]any, fields []string) error {
	if sample == nil {
		return nil
	}
	for _, f := range fields {
		if _, ok := sample[f]; !ok {
			available := make([]string, 0, len(sample))
			for k := range sample {
				available = append(available, k)
			}
			sort.Strings(available)
			return cli.Errorf("--fields: unknown field %q\navailable: %s", f, strings.Join(available, ", "))
		}
	}
	return nil
}

// pickFields returns a new map with only the requested keys, preserving the
// caller-specified order via an ordered map encoded as a slice of pairs.
// Since Go maps don't preserve insertion order, we round-trip through a
// linked structure: easier to just emit JSON directly with explicit order.
func pickFields(obj map[string]any, fields []string) map[string]any {
	if obj == nil {
		return nil
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := obj[f]; ok {
			out[f] = v
		}
	}
	return out
}

// writeFormat marshals an already-projected value in the chosen format.
func writeFormat(ctx *cli.Context, format string, projected any) error {
	switch format {
	case "json", "pretty":
		// Pretty falls back to JSON when --fields is set: there's no
		// reasonable "table of arbitrary projected scalars" view that beats
		// JSON for clarity, and JSON is what the user shaped.
		buf, err := json.MarshalIndent(projected, "", "  ")
		if err != nil {
			return err
		}
		ctx.Println(string(buf))
		return nil
	case "yaml":
		buf, err := yaml.Marshal(projected)
		if err != nil {
			return err
		}
		ctx.Println(strings.TrimRight(string(buf), "\n"))
		return nil
	case "text":
		return writeProjectedText(ctx, projected)
	default:
		return cli.Errorf("--output %q: expected one of auto, pretty, json, yaml, text", format)
	}
}

// writeFormatRaw formats the response without a projection. For text we
// fall back to the generic auto-picked columns (same logic as the pretty
// renderer's table).
func writeFormatRaw(ctx *cli.Context, format string, body []byte) error {
	switch format {
	case "pretty":
		return renderPretty(ctx, body)
	case "json":
		var tmp any
		if err := json.Unmarshal(body, &tmp); err != nil {
			ctx.Println(string(body))
			return nil
		}
		buf, err := json.MarshalIndent(tmp, "", "  ")
		if err != nil {
			return err
		}
		ctx.Println(string(buf))
		return nil
	case "yaml":
		var tmp any
		if err := json.Unmarshal(body, &tmp); err != nil {
			ctx.Println(string(body))
			return nil
		}
		buf, err := yaml.Marshal(tmp)
		if err != nil {
			return err
		}
		ctx.Println(strings.TrimRight(string(buf), "\n"))
		return nil
	case "text":
		return writeAutoText(ctx, body)
	default:
		return cli.Errorf("--output %q: expected one of auto, pretty, json, yaml, text", format)
	}
}

// writeProjectedText prints projected data as tab-separated rows. Lists get
// one row per item; single resources get one row. Field order matches the
// --fields flag (its argument was carried through projectBody → pickFields,
// but Go map iteration is unordered, so we re-derive order here from the
// keys present in the projected value).
func writeProjectedText(ctx *cli.Context, projected any) error {
	rows := projectedRows(projected)
	for _, row := range rows {
		// Stable column order: sort keys alphabetically. Callers who care
		// about column order should rely on -o yaml/json instead — text is
		// for ad-hoc piping.
		keys := make([]string, 0, len(row))
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		cells := make([]string, len(keys))
		for i, k := range keys {
			cells[i] = fieldString(row[k])
		}
		ctx.Println(strings.Join(cells, "\t"))
	}
	return nil
}

// projectedRows extracts a flat slice of row-maps from a projected value:
// envelope → items[], resource → [resource]. Anything else returns empty.
func projectedRows(projected any) []map[string]any {
	obj, ok := projected.(map[string]any)
	if !ok {
		return nil
	}
	if items, isList := obj["items"].([]any); isList {
		out := make([]map[string]any, 0, len(items))
		for _, it := range items {
			if row, ok := it.(map[string]any); ok {
				out = append(out, row)
			}
		}
		return out
	}
	return []map[string]any{obj}
}

// writeAutoText emits tab-separated rows using the generic column picker
// when no --fields is set. Same column choice as the pretty table.
func writeAutoText(ctx *cli.Context, body []byte) error {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		ctx.Println(string(body))
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		ctx.Println(string(body))
		return nil
	}
	if items, isList := obj["items"].([]any); isList {
		if len(items) == 0 {
			return nil
		}
		cols := pickColumns(items)
		for _, it := range items {
			row, _ := it.(map[string]any)
			cells := make([]string, len(cols))
			for i, c := range cols {
				cells[i] = fieldString(row[c])
			}
			ctx.Println(strings.Join(cells, "\t"))
		}
		return nil
	}
	keys := orderedKeys(obj)
	cells := make([]string, 0, len(keys))
	for _, k := range keys {
		if isScalar(obj[k]) {
			cells = append(cells, fieldString(obj[k]))
		}
	}
	ctx.Println(strings.Join(cells, "\t"))
	return nil
}

// fieldString stringifies a JSON value for tab-separated output. Nested
// objects/arrays are JSON-encoded so each row stays single-line.
func fieldString(v any) string {
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
	case json.Number:
		return string(x)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	default:
		buf, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(buf)
	}
}

// marshalForOutput renders an arbitrary value (typically a request body)
// according to --output. Used by --dry-run.
func marshalForOutput(ctx *cli.Context, v any) ([]byte, error) {
	mode := strings.ToLower(ctx.String("output"))
	if mode == "yaml" {
		return yaml.Marshal(v)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// parseIntArg parses a positional int argument, returning a friendly error
// when the value is not a valid integer.
func parseIntArg(s, name string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid %s: %q is not an integer", name, s)
	}
	return n, nil
}
`)
}
