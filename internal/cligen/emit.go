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
	GoName   string // signature argument name (e.g. "body")
	TypeName string // e.g. "CreateChannelJSONRequestBody"
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
			pc.Body = &BodyArg{GoName: p.Name, TypeName: p.Type}
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
		"disconnect", "handle",
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

	var b bytes.Buffer
	fmt.Fprintf(&b, `// Code generated by mobius-cligen. DO NOT EDIT.
//
// Regenerate with:  make generate-go-cli
//
// To suppress or override a command, edit
// cmd/mobius-cligen/overrides.go — never hand-edit this file.

package main

import (
	"github.com/deepnoodle-ai/wonton/cli"
`)
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
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/deepnoodle-ai/wonton/cli"
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
			help := firstNonEmpty(f.Description, f.FlagName)
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
		flags = append(flags, `cli.String("file", "f").Help("Request body as JSON (path to file, or '-' for stdin)")`)
	}
	if len(flags) > 0 {
		fmt.Fprintf(b, "\t\tFlags(\n")
		for _, f := range flags {
			fmt.Fprintf(b, "\t\t\t%s,\n", f)
		}
		fmt.Fprintf(b, "\t\t).\n")
	}

	fmt.Fprintf(b, "\t\tUse(cli.RequireFlags(\"api-key\")).\n")
	fmt.Fprintf(b, "\t\tRun(func(ctx *cli.Context) error {\n")
	fmt.Fprintf(b, "\t\t\tmc, err := clientFromContext(ctx)\n")
	fmt.Fprintf(b, "\t\t\tif err != nil { return err }\n")
	fmt.Fprintf(b, "\t\t\tclient := mc.RawClient()\n")

	// Collect path-param locals. Positional path params consume args in
	// order; flag-backed ones (e.g. --project) read from their flag.
	argIdx := 0
	for i, p := range c.PathParams {
		switch {
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
	fmt.Fprintf(b, "\t\t\treturn printResponse(ctx, resp.StatusCode(), resp.Body)\n")
	fmt.Fprintf(b, "\t\t})\n\n")
	return nil
}

// renderHelpers writes the runtime helpers the generated commands rely on.
func renderHelpers(b *bytes.Buffer) {
	b.WriteString(`// readJSONBody reads a JSON request body from --file (path or '-' for stdin)
// and unmarshals it into v.
func readJSONBody(ctx *cli.Context, v any) error {
	path := ctx.String("file")
	if path == "" {
		return fmt.Errorf("missing --file (use '-' to read JSON from stdin)")
	}
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(ctx.Stdin())
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// printResponse prints a pretty JSON rendering of the response body to stdout.
// Non-2xx responses are printed as-is and returned as an error so callers exit
// with a non-zero status.
func printResponse(ctx *cli.Context, status int, body []byte) error {
	var pretty []byte
	if len(body) > 0 && (body[0] == '{' || body[0] == '[') {
		var tmp any
		if err := json.Unmarshal(body, &tmp); err == nil {
			pretty, _ = json.MarshalIndent(tmp, "", "  ")
		}
	}
	if pretty == nil {
		pretty = body
	}
	if status >= 200 && status < 300 {
		ctx.Println(string(pretty))
		return nil
	}
	return fmt.Errorf("HTTP %d: %s", status, string(pretty))
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
