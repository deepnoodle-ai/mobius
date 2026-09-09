package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// registerSessionAttachCommand adds the streaming upload path that the
// generated commands cannot express: the createSessionAttachment operation is
// multipart, so a real file (or stdin) source is hand-written here.
func registerSessionAttachCommand(app *cli.App) {
	grp := app.Group("sessions")
	grp.Command("attach").
		Description("Attach a document or image to a session").
		AddArg(&cli.Arg{Name: "session-id", Description: "Session to attach the file to.", Required: true}).
		AddArg(&cli.Arg{Name: "path", Description: "File to attach ('-' for stdin).", Required: true}).
		Flags(
			cli.String("name", "").Help("Display filename. Defaults to the file's base name."),
			cli.String("mime", "").Help("MIME hint. The server detects the real media type from the bytes."),
			cli.String("idempotency-key", "").Help("Retry key scoped to this session and caller (255 chars max)."),
		).
		Use(requireAuth()).
		Run(runSessionAttach)
}

func runSessionAttach(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	opts := mobius.CreateSessionAttachmentOptions{
		Name:           ctx.String("name"),
		Mime:           ctx.String("mime"),
		IdempotencyKey: ctx.String("idempotency-key"),
	}
	if path := ctx.Arg(1); path == "-" {
		if strings.TrimSpace(opts.Name) == "" {
			return fmt.Errorf("--name is required when attaching from stdin")
		}
		opts.Reader = ctx.Stdin()
	} else {
		opts.Path = path
		if opts.Mime == "" {
			opts.Mime = inferMimeType(path)
		}
	}
	attachment, err := mc.CreateSessionAttachment(ctx.Context(), ctx.Arg(0), opts)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(attachment)
	if err != nil {
		return err
	}
	return printResponse(ctx, "createSessionAttachment", http.StatusCreated, raw)
}
