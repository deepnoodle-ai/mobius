package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// registerArtifactUploadCommand adds the streaming upload path that the
// generated commands cannot express: the createArtifact operation is
// multipart, so a real file (or stdin) source is hand-written here.
func registerArtifactUploadCommand(app *cli.App) {
	grp := app.Group("artifacts")
	grp.Command("upload").
		Description("Upload a file as a private project artifact").
		AddArg(&cli.Arg{Name: "path", Description: "File to upload ('-' for stdin).", Required: true}).
		Flags(
			cli.String("name", "").Help("Display name or relative virtual path (e.g. renders/report.html). Defaults to the file's base name."),
			cli.String("mime", "").Help("MIME type override. Defaults to a type inferred from the file extension."),
			cli.String("metadata", "").Help("Caller metadata JSON object. Accepts JSON, @file, or @-."),
			cli.String("idempotency-key", "").Help("Durable retry key for this one artifact (255 chars max)."),
		).
		Use(requireAuth()).
		Run(runArtifactUpload)
}

func runArtifactUpload(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	opts := mobius.CreateArtifactOptions{
		Name:           ctx.String("name"),
		Mime:           ctx.String("mime"),
		IdempotencyKey: ctx.String("idempotency-key"),
	}
	if ctx.IsSet("metadata") {
		if err := decodeFlagJSON(ctx, "metadata", ctx.String("metadata"), &opts.Metadata); err != nil {
			return err
		}
	}
	if path := ctx.Arg(0); path == "-" {
		if strings.TrimSpace(opts.Name) == "" {
			return fmt.Errorf("--name is required when uploading from stdin")
		}
		opts.Reader = ctx.Stdin()
	} else {
		opts.Path = path
		if opts.Mime == "" {
			opts.Mime = inferMimeType(path)
		}
	}
	artifact, err := mc.CreateArtifact(ctx.Context(), opts)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	return printResponse(ctx, "createArtifact", http.StatusCreated, raw)
}

// inferMimeType guesses a MIME type from the file extension, without
// parameters (the server records the bare type). Returns "" when unknown so
// the server applies its own default.
func inferMimeType(path string) string {
	byExt := mime.TypeByExtension(filepath.Ext(path))
	if byExt == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(byExt)
	if err != nil {
		return ""
	}
	return mediaType
}
