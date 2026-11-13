package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// pyHelper is the `mobius` Python package a managed worker puts on
// PYTHONPATH, so `from mobius import actions` works in every script step.
//
//go:embed pyhelper/mobius/*.py
var pyHelper embed.FS

// registerScriptActionCommands adds `mobius actions run` and `mobius
// actions describe`. Both work only inside a job of a managed environment:
// they talk to the worker's action socket, never to the API, and carry no
// credential.
func registerScriptActionCommands(app *cli.App) {
	grp := app.Group("actions")
	grp.Command("run").
		Description("Run an action from a workflow script step").
		Long("Runs one of the step's declared actions as the workflow and prints its output as JSON. "+
			"Works only inside a workflow script step.").
		Args("action").
		Flags(
			cli.String("input", "").Help("The action's input as JSON, @file, or - (or @-) for stdin."),
			cli.String("key", "").Help("Call key. A repeat in this step, across its attempts in one run, returns the first result and runs nothing."),
		).
		Run(runScriptAction)
	grp.Command("describe").
		Description("Describe an action's inputs and output").
		Long("Prints an action's title, whether it changes anything, its inputs, and its output shape. " +
			"Works only inside a job in a Mobius computer.").
		Args("action").
		Flags(cli.Bool("json", "").Help("Print the description as JSON.")).
		Run(describeScriptAction)
}

func runScriptAction(ctx *cli.Context) error {
	input := map[string]any{}
	if raw := strings.TrimSpace(ctx.String("input")); raw != "" {
		if raw == "-" {
			raw = "@-"
		}
		if err := decodeFlagJSON(ctx, "input", raw, &input); err != nil {
			return err
		}
	}
	resp, err := callActionSocket(ctx.Context(), actionSocketRequest{
		Op:     "run",
		Action: ctx.Arg(0),
		Input:  input,
		Key:    ctx.String("key"),
	})
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(resp.Output, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(ctx.Stdout(), string(out))
	return err
}

func describeScriptAction(ctx *cli.Context) error {
	resp, err := callActionSocket(ctx.Context(), actionSocketRequest{Op: "describe", Action: ctx.Arg(0)})
	if err != nil {
		return err
	}
	if ctx.Bool("json") {
		out, err := json.MarshalIndent(resp.Description, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(ctx.Stdout(), string(out))
		return err
	}
	_, err = io.WriteString(ctx.Stdout(), formatActionDescription(resp.Description))
	return err
}

// callActionSocket sends one request to the worker's action socket and
// returns its answer. A refusal comes back as an error with the worker's
// sentence.
func callActionSocket(ctx context.Context, req actionSocketRequest) (*actionSocketResponse, error) {
	if strings.TrimSpace(req.Action) == "" {
		return nil, cli.Errorf("name an action, for example slack.message.post")
	}
	path := strings.TrimSpace(os.Getenv(actionSocketEnv))
	if path == "" {
		return nil, cli.Errorf("mobius actions %s works only inside a job in a Mobius computer", req.Op)
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("connect to the Mobius worker: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(actionSocketTimeout))
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, fmt.Errorf("send to the Mobius worker: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read from the Mobius worker: %w", err)
	}
	var resp actionSocketResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("the Mobius worker sent an unreadable answer")
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return &resp, nil
}

// formatActionDescription renders a description for a person at a
// terminal: the title and effect, the inputs, the output fields, and a
// command to copy.
func formatActionDescription(d *mobius.ScriptActionDescription) string {
	if d == nil {
		return ""
	}
	var b strings.Builder
	effect := "reads only"
	if d.Effect == "changes" {
		effect = "changes something"
	}
	title := d.Title
	if title == "" {
		title = d.Name
	}
	fmt.Fprintf(&b, "%s (%s): %s\n", title, d.Name, effect)
	if d.Description != "" {
		fmt.Fprintf(&b, "%s\n", d.Description)
	}
	b.WriteString("\nInputs:\n")
	if len(d.Inputs) == 0 {
		b.WriteString("  none\n")
	}
	example := map[string]any{}
	for _, in := range d.Inputs {
		kind := in.Type
		if kind == "" {
			kind = "any"
		}
		required := ""
		if in.Required {
			required = ", required"
			example[in.Name] = exampleValue(kind)
		}
		fmt.Fprintf(&b, "  %s (%s%s)", in.Name, kind, required)
		if in.Description != "" {
			fmt.Fprintf(&b, ": %s", in.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nOutput:\n")
	props, _ := d.OutputSchema["properties"].(map[string]any)
	if len(props) == 0 {
		b.WriteString("  any JSON value\n")
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prop, _ := props[name].(map[string]any)
		kind, _ := prop["type"].(string)
		if kind == "" {
			kind = "any"
		}
		fmt.Fprintf(&b, "  %s (%s)\n", name, kind)
	}
	raw, _ := json.Marshal(example)
	fmt.Fprintf(&b, "\nRun it:\n  mobius actions run %s --input '%s'\n", d.Name, string(raw))
	return b.String()
}

func exampleValue(kind string) any {
	switch kind {
	case "integer", "number":
		return 0
	case "boolean":
		return false
	case "object":
		return map[string]any{}
	case "array":
		return []any{}
	}
	return "…"
}

// installScriptHelpers makes `mobius` and the Python helper available to
// every job shell: it writes the helper to a private directory, puts it
// first on PYTHONPATH, and puts this binary's directory on PATH.
func installScriptHelpers() (cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "mobius-py-")
	if err != nil {
		return nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	err = fs.WalkDir(pyHelper, "pyhelper", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := pyHelper.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, strings.TrimPrefix(path, "pyhelper/"))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := os.Setenv("PYTHONPATH", prependPath(dir, os.Getenv("PYTHONPATH"))); err != nil {
		cleanup()
		return nil, err
	}
	if exe, exeErr := os.Executable(); exeErr == nil {
		if err := os.Setenv("PATH", prependPath(filepath.Dir(exe), os.Getenv("PATH"))); err != nil {
			cleanup()
			return nil, err
		}
	}
	return cleanup, nil
}

// prependPath puts dir first in a list of paths, once.
func prependPath(dir, list string) string {
	parts := []string{dir}
	for _, p := range filepath.SplitList(list) {
		if p != "" && p != dir {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}
