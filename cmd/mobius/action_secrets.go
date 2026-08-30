package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerActionSecretCommands adds the create and rotate commands that
// reveal one-time secret material. Each requires an explicit output sink so
// the reveal is never printed by accident.
func registerActionSecretCommands(app *cli.App) {
	grp := app.Group("actions")
	secretSinkFlags := []cli.Flag{
		cli.String("secret-file", "").Help("Write the one-time whsec_ signing secret to this file (created 0600) and mask it in the printed response."),
		cli.Bool("show-secret", "").Help("Print the one-time signing secret in the response instead of saving it to a file."),
		cli.Bool("force", "").Help("Allow --secret-file to overwrite an existing file."),
	}
	createFlags := []cli.Flag{
		cli.String("annotations", "").Help("Request hints that describe the safe-use properties of the action. Accepts JSON, @file, or @-."),
		cli.String("description", "").Help("Markdown-safe description of what the action does."),
		cli.String("endpoint-kind", "").Help("Backing kind for the action: http or worker."),
		cli.String("endpoint-url", "").Help("Required when endpoint-kind is http."),
		cli.String("input-schema", "").Help("JSON Schema describing expected inputs. Accepts JSON, @file, or @-."),
		cli.String("invocation-format", "").Help("HTTP request-body contract."),
		cli.String("name", "").Help("[required] Identifier used in loop step definitions."),
		cli.String("output-schema", "").Help("JSON Schema describing expected output. Accepts JSON, @file, or @-."),
		cli.String("owner", "").Help("Resource owner. Accepts JSON, @file, or @-."),
		cli.Strings("tag", "").Help("Tag in KEY=VALUE form. Repeatable."),
		cli.String("title", "").Help("Human-readable display name."),
		cli.String("visibility", "").Help("Who the custodian chose to share the resource with."),
		cli.String("file", "f").Help("Request body from a file (JSON or YAML, '-' for stdin). Flags override file contents."),
		cli.Bool("dry-run", "").Help("Print the assembled request body and exit without sending it."),
	}

	grp.Command("create").
		Description("Create action").
		Flags(append(createFlags, secretSinkFlags...)...).
		Use(requireAuth()).
		Run(runActionCreate)

	grp.Command("rotate-secret").
		Description("Rotate signing secret").
		Args("action-name").
		Flags(secretSinkFlags...).
		Use(requireAuth()).
		Run(runActionRotateSecret)
}

func runActionCreate(ctx *cli.Context) error {
	var body api.CreateActionJSONRequestBody
	if err := readJSONBody(ctx, &body); err != nil {
		return err
	}
	if ctx.IsSet("annotations") {
		if err := decodeFlagJSON(ctx, "annotations", ctx.String("annotations"), &body.Annotations); err != nil {
			return err
		}
	}
	if ctx.IsSet("description") {
		v := ctx.String("description")
		body.Description = &v
	}
	if ctx.IsSet("endpoint-kind") {
		v := api.ActionEndpointKind(ctx.String("endpoint-kind"))
		body.EndpointKind = &v
	}
	if ctx.IsSet("endpoint-url") {
		v := ctx.String("endpoint-url")
		body.EndpointUrl = &v
	}
	if ctx.IsSet("input-schema") {
		if err := decodeFlagJSON(ctx, "input-schema", ctx.String("input-schema"), &body.InputSchema); err != nil {
			return err
		}
	}
	if ctx.IsSet("invocation-format") {
		v := api.ActionInvocationFormat(ctx.String("invocation-format"))
		body.InvocationFormat = &v
	}
	if ctx.IsSet("name") {
		body.Name = ctx.String("name")
	}
	if ctx.IsSet("output-schema") {
		if err := decodeFlagJSON(ctx, "output-schema", ctx.String("output-schema"), &body.OutputSchema); err != nil {
			return err
		}
	}
	if ctx.IsSet("owner") {
		if err := decodeFlagJSON(ctx, "owner", ctx.String("owner"), &body.Owner); err != nil {
			return err
		}
	}
	if tags, err := parseTagFlags(ctx); err != nil {
		return err
	} else if tags != nil {
		v := api.TagMap(tags)
		body.Tags = &v
	}
	if ctx.IsSet("title") {
		v := ctx.String("title")
		body.Title = &v
	}
	if ctx.IsSet("visibility") {
		v := api.ResourceVisibility(ctx.String("visibility"))
		body.Visibility = &v
	}
	if body.Name == "" {
		return fmt.Errorf("--name is required (or supply it via --file)")
	}
	if ctx.Bool("dry-run") {
		return printDryRun(ctx, body, "annotations", "input_schema", "output_schema", "owner", "tags")
	}
	sink, err := secretSinkFromFlags(ctx)
	if err != nil {
		return err
	}
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	resp, err := mc.RawClient().CreateActionWithResponse(ctx.Context(), body)
	if err != nil {
		return err
	}
	return sink.deliver(ctx, "createAction", resp.StatusCode(), resp.Body)
}

func runActionRotateSecret(ctx *cli.Context) error {
	sink, err := secretSinkFromFlags(ctx)
	if err != nil {
		return err
	}
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	resp, err := mc.RawClient().RotateActionSecretWithResponse(ctx.Context(), ctx.Arg(0))
	if err != nil {
		return err
	}
	return sink.deliver(ctx, "rotateActionSecret", resp.StatusCode(), resp.Body)
}

type secretSink struct {
	file  string
	show  bool
	force bool
}

func secretSinkFromFlags(ctx *cli.Context) (*secretSink, error) {
	s := &secretSink{file: ctx.String("secret-file"), show: ctx.Bool("show-secret"), force: ctx.Bool("force")}
	switch {
	case s.file != "" && s.show:
		return nil, fmt.Errorf("--secret-file and --show-secret are mutually exclusive")
	case s.file == "" && !s.show:
		return nil, fmt.Errorf("the signing secret is revealed exactly once: pass --secret-file PATH to save it, or --show-secret to print it")
	case s.force && s.file == "":
		return nil, fmt.Errorf("--force only applies with --secret-file")
	}
	if s.file != "" && !s.force {
		if _, err := os.Lstat(s.file); err == nil {
			return nil, fmt.Errorf("%s already exists; pass --force to overwrite", s.file)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("check %s: %w", s.file, err)
		}
	}
	return s, nil
}

func (s *secretSink) deliver(ctx *cli.Context, opID string, status int, body []byte) error {
	if status < 200 || status >= 300 || s.show {
		return printResponse(ctx, opID, status, body)
	}
	var masked map[string]any
	if err := json.Unmarshal(body, &masked); err != nil {
		return fmt.Errorf("%s: parse response: %w", opID, err)
	}
	secret, ok := masked["signing_secret"].(string)
	if !ok || secret == "" {
		return fmt.Errorf("%s: response did not include the one-time signing_secret; nothing was written to %s", opID, s.file)
	}
	delete(masked, "signing_secret")
	maskedBody, err := json.Marshal(masked)
	if err != nil {
		return fmt.Errorf("%s: render masked response: %w", opID, err)
	}
	if err := writeSecretFile(s.file, []byte(secret+"\n"), s.force); err != nil {
		return fmt.Errorf("%s: the signing secret could not be saved (%v); it was NOT printed and cannot be retrieved again — rotate the secret and retry", opID, err)
	}
	if err := printResponse(ctx, opID, status, maskedBody); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stderr(), "Wrote one-time signing secret to %s\n", s.file)
	return nil
}

func writeSecretFile(path string, data []byte, force bool) error {
	if !force {
		return os.WriteFile(path, data, 0o600)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
