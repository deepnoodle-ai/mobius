package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerOrgActionSecretCommands adds the two org-action commands that
// reveal one-time secret material. They are hand-written (the generator
// skips createOrganizationAction and rotateOrganizationActionSecret) so the
// signing secret never reaches the terminal by accident: each command
// requires an explicit sink — --secret-file to save the key, or
// --show-secret to print the full response.
func registerOrgActionSecretCommands(app *cli.App) {
	grp := app.Group("org-actions")

	secretSinkFlags := []cli.Flag{
		cli.String("secret-file", "").Help("Write the base64-encoded signing secret to this file (created 0600) and mask it in the printed response."),
		cli.Bool("show-secret", "").Help("Print the one-time signing secret in the response instead of saving it to a file."),
		cli.Bool("force", "").Help("Allow --secret-file to overwrite an existing file."),
	}

	createFlags := []cli.Flag{
		cli.String("annotations", "").Help("Request hints that describe the safe-use properties of the action. Accepts JSON, @file, or @-."),
		cli.String("description", "").Help("description"),
		cli.Bool("enabled", "").Help("enabled"),
		cli.String("endpoint-url", "").Help("[required] Public HTTPS endpoint. Private, loopback, link-local, and redirect targets are rejected."),
		cli.String("input-schema", "").Help("input-schema Accepts JSON, @file, or @-."),
		cli.String("invocation-format", "").Help("invocation-format"),
		cli.String("name", "").Help("[required] Canonical dotted name selected by project toolkits."),
		cli.String("output-schema", "").Help("output-schema Accepts JSON, @file, or @-."),
		cli.String("title", "").Help("title"),
		cli.String("file", "f").Help("Request body from a file (JSON or YAML, '-' for stdin). Flags override file contents."),
		cli.Bool("dry-run", "").Help("Print the assembled request body and exit without sending it."),
	}

	grp.Command("create").
		Description("Create organization action").
		Flags(append(createFlags, secretSinkFlags...)...).
		Use(requireAuth()).
		Run(runOrgActionCreate)

	grp.Command("rotate-secret").
		Description("Rotate an organization action secret").
		Args("action-id").
		Flags(secretSinkFlags...).
		Use(requireAuth()).
		Run(runOrgActionRotateSecret)
}

func runOrgActionCreate(ctx *cli.Context) error {
	var body api.CreateOrganizationActionJSONRequestBody
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
	if ctx.IsSet("enabled") {
		v := ctx.Bool("enabled")
		body.Enabled = &v
	}
	if ctx.IsSet("endpoint-url") {
		body.EndpointUrl = ctx.String("endpoint-url")
	}
	if ctx.IsSet("input-schema") {
		if err := decodeFlagJSON(ctx, "input-schema", ctx.String("input-schema"), &body.InputSchema); err != nil {
			return err
		}
	}
	if ctx.IsSet("invocation-format") {
		v := api.CreateOrganizationActionRequestInvocationFormat(ctx.String("invocation-format"))
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
	if ctx.IsSet("title") {
		v := ctx.String("title")
		body.Title = &v
	}
	if body.EndpointUrl == "" {
		return fmt.Errorf("--endpoint-url is required (or supply it via --file)")
	}
	if body.Name == "" {
		return fmt.Errorf("--name is required (or supply it via --file)")
	}
	if ctx.Bool("dry-run") {
		return printDryRun(ctx, body)
	}
	sink, err := secretSinkFromFlags(ctx)
	if err != nil {
		return err
	}
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	resp, err := mc.RawClient().CreateOrganizationActionWithResponse(ctx.Context(), body)
	if err != nil {
		return err
	}
	return sink.deliver(ctx, "createOrganizationAction", resp.StatusCode(), resp.Body)
}

func runOrgActionRotateSecret(ctx *cli.Context) error {
	sink, err := secretSinkFromFlags(ctx)
	if err != nil {
		return err
	}
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	resp, err := mc.RawClient().RotateOrganizationActionSecretWithResponse(ctx.Context(), ctx.Arg(0))
	if err != nil {
		return err
	}
	return sink.deliver(ctx, "rotateOrganizationActionSecret", resp.StatusCode(), resp.Body)
}

// secretSink is the caller-chosen destination for one-time secret material.
type secretSink struct {
	file  string
	show  bool
	force bool
}

// secretSinkFromFlags validates the sink flags before any API call is made,
// so a refused invocation never burns the one-time reveal.
func secretSinkFromFlags(ctx *cli.Context) (*secretSink, error) {
	s := &secretSink{
		file:  ctx.String("secret-file"),
		show:  ctx.Bool("show-secret"),
		force: ctx.Bool("force"),
	}
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

// deliver routes a create/rotate response to the chosen sink. Error responses
// pass through untouched (they carry no secret). With --show-secret the full
// body is printed as-is; with --secret-file the base64 signing_secret is
// written to the file and masked out of the printed response, followed by a
// stderr summary of where the key went.
func (s *secretSink) deliver(ctx *cli.Context, opID string, status int, body []byte) error {
	if status < 200 || status >= 300 || s.show {
		return printResponse(ctx, opID, status, body)
	}

	var action api.OrganizationAction
	if err := json.Unmarshal(body, &action); err != nil {
		return fmt.Errorf("%s: parse response: %w", opID, err)
	}
	if action.SigningSecret == nil || *action.SigningSecret == "" {
		return fmt.Errorf("%s: response did not include the one-time signing_secret; nothing was written to %s", opID, s.file)
	}
	secret := *action.SigningSecret

	var masked map[string]any
	if err := json.Unmarshal(body, &masked); err != nil {
		return fmt.Errorf("%s: parse response: %w", opID, err)
	}
	delete(masked, "signing_secret")
	maskedBody, err := json.Marshal(masked)
	if err != nil {
		return fmt.Errorf("%s: render masked response: %w", opID, err)
	}

	// The value on disk is the API-returned base64 encoding of the key, plus
	// a trailing newline. Written before rendering so a print failure cannot
	// lose the secret.
	if err := os.WriteFile(s.file, []byte(secret+"\n"), 0o600); err != nil {
		return fmt.Errorf("%s: the signing secret could not be saved (%v); it was NOT printed and cannot be retrieved again — rotate the secret and retry", opID, err)
	}
	if s.force {
		// WriteFile keeps an existing file's permissions; tighten them.
		if err := os.Chmod(s.file, 0o600); err != nil {
			return fmt.Errorf("%s: chmod %s: %w", opID, s.file, err)
		}
	}

	if err := printResponse(ctx, opID, status, maskedBody); err != nil {
		return err
	}
	version := int64(0)
	for _, v := range action.SecretVersions {
		if v.Version > version {
			version = v.Version
		}
	}
	fmt.Fprintf(ctx.Stderr(), "Wrote base64-encoded signing secret for action %s (secret_ref %s, version %d) to %s\n",
		action.Id, action.SecretRef, version, s.file)
	return nil
}
