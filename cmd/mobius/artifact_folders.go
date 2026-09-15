package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// folderIDPrefix is the ID prefix the server assigns declared folders. It is
// what tells `rmdir afd_123` from `rmdir reports/2026`.
const folderIDPrefix = "afd_"

// registerArtifactFolderCommands adds the Library folder commands. A folder
// is the directory part of an artifact name, so these read as their
// filesystem counterparts; the generator skips the three operations
// (updateArtifact, createArtifactFolder, deleteArtifactFolder) because each
// one wants a positional argument that its request shape cannot express.
func registerArtifactFolderCommands(app *cli.App) {
	grp := app.Group("artifacts")

	grp.Command("move").
		Description("Move or rename a file").
		Long("Changes an artifact's name. The name is a relative path, so a leading " +
			"folder is what places the file: `reports/2026/weekly.md` moves it into " +
			"`reports/2026`, and a bare `weekly.md` moves it to the top level of the " +
			"Library. The file's whole version history moves with it, so its past " +
			"versions never sit in a different folder. The ID, custody, visibility, " +
			"and stored bytes are unchanged.").
		AddArg(&cli.Arg{Name: "artifact-id", Description: "ID of the artifact", Required: true}).
		AddArg(&cli.Arg{Name: "new-name", Description: "New name as a relative path; a leading folder places the file.", Required: true}).
		Use(requireAuth()).
		Run(runArtifactMove)

	grp.Command("mkdir").
		Description("Create folder").
		Long("Declares a folder so it can exist before any file is in it. The call " +
			"is idempotent: declaring a folder that already exists returns the " +
			"existing one. A declared folder holds nothing itself, charges no " +
			"quota, and never widens access to any file.").
		AddArg(&cli.Arg{Name: "path", Description: "Folder path, relative and with no leading or trailing slash (e.g. reports/2026).", Required: true}).
		Flags(
			cli.String("visibility", "").Help("Who the folder is shared with: organization (default) or private."),
		).
		Use(requireAuth()).
		Run(runArtifactMkdir)

	grp.Command("rmdir").
		Description("Delete folder").
		Long("Removes a declared folder, named by ID or by path. The folder must be " +
			"empty: the call is refused while any visible file lives in it or below " +
			"it, or while a declared subfolder remains. Files are never deleted by " +
			"this command.").
		AddArg(&cli.Arg{Name: "folder", Description: "Folder ID (afd_...) or path (e.g. reports/2026).", Required: true}).
		Use(requireAuth()).
		Run(runArtifactRmdir)
}

func init() {
	// The generic renderer would lead with the folder ID, which is absent for
	// an implied folder and is not how anyone reads a folder listing. Name,
	// path, and whether the folder is declared are the three facts that
	// matter — declared is what says whether `rmdir` can remove it.
	RegisterResponseRenderer("listArtifactFolders", func(ctx *cli.Context, body []byte) error {
		return renderPretty(ctx, body, "name", "path", "declared")
	})
}

func runArtifactMove(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	name := ctx.Arg(1)
	if strings.TrimSpace(name) == "" {
		return cli.Errorf("new name is required")
	}
	resp, err := mc.RawClient().UpdateArtifactWithResponse(
		ctx.Context(), api.ArtifactIdParam(ctx.Arg(0)),
		api.UpdateArtifactJSONRequestBody{Name: name},
	)
	if err != nil {
		return err
	}
	return printResponse(ctx, "updateArtifact", resp.StatusCode(), resp.Body)
}

func runArtifactMkdir(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	path := strings.TrimSpace(ctx.Arg(0))
	if path == "" {
		return cli.Errorf("folder path is required")
	}
	body := api.CreateArtifactFolderJSONRequestBody{Path: path}
	if ctx.IsSet("visibility") {
		v := api.ResourceVisibility(ctx.String("visibility"))
		body.Visibility = &v
	}
	resp, err := mc.RawClient().CreateArtifactFolderWithResponse(ctx.Context(), body)
	if err != nil {
		return err
	}
	return printResponse(ctx, "createArtifactFolder", resp.StatusCode(), resp.Body)
}

func runArtifactRmdir(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	ref := strings.TrimSpace(ctx.Arg(0))
	if ref == "" {
		return cli.Errorf("folder ID or path is required")
	}
	folderID, err := resolveArtifactFolderID(ctx, ref)
	if err != nil {
		return err
	}
	resp, err := client.DeleteArtifactFolderWithResponse(ctx.Context(), api.ArtifactFolderIdParam(folderID))
	if err != nil {
		return err
	}
	if resp.StatusCode() == http.StatusConflict {
		if msg := folderNotEmptyMessage(ref, resp.Body); msg != "" {
			return &cli.ExitError{Code: exitCodeForStatus(resp.StatusCode()), Message: msg}
		}
	}
	return printResponse(ctx, "deleteArtifactFolder", resp.StatusCode(), resp.Body)
}

// resolveArtifactFolderID turns a `rmdir` argument into a folder ID. An
// afd_-prefixed argument is already one; a path is looked up among its
// parent's children, which is also how the caller learns that the folder is
// only implied by the names of the files in it and so has nothing to remove.
func resolveArtifactFolderID(ctx *cli.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, folderIDPrefix) {
		return ref, nil
	}
	mc, err := clientFromContext(ctx)
	if err != nil {
		return "", err
	}
	path := strings.Trim(ref, "/")
	if path == "" {
		return "", cli.Errorf("the top level of the Library is not a folder and cannot be removed")
	}
	params := &api.ListArtifactFoldersParams{}
	if parent := parentFolderPath(path); parent != "" {
		params.Parent = &parent
	}
	resp, err := mc.RawClient().ListArtifactFoldersWithResponse(ctx.Context(), params)
	if err != nil {
		return "", err
	}
	if resp.StatusCode() != http.StatusOK {
		if err := printResponse(ctx, "listArtifactFolders", resp.StatusCode(), resp.Body); err != nil {
			return "", err
		}
		return "", cli.Errorf("could not resolve folder %q", path)
	}
	var list api.ArtifactFolderListResponse
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return "", cli.Errorf("could not resolve folder %q: %v", path, err)
	}
	for _, item := range list.Items {
		if item.Path != path {
			continue
		}
		if !item.Declared || item.Id == nil {
			return "", cli.Errorf("folder %q is implied by the names of the files in it, not declared, so there is nothing to remove; move or delete those files instead", path)
		}
		return *item.Id, nil
	}
	return "", cli.Errorf("no folder %q", path)
}

// parentFolderPath returns the containing folder of a normalized path, or ""
// when the path sits at the top level of the Library.
func parentFolderPath(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return ""
}

// folderNotEmptyMessage turns a `folder_not_empty` refusal into the sentence
// the caller needs — how many files are in the way — instead of the raw error
// body. Returns "" when the response is some other conflict, which then
// prints through the normal path.
func folderNotEmptyMessage(ref string, body []byte) string {
	var resp api.ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	if resp.Error.Code != "folder_not_empty" {
		return ""
	}
	count, ok := folderFileCount(resp.Error.Details)
	if !ok {
		return resp.Error.Message
	}
	files := "files"
	if count == 1 {
		files = "file"
	}
	return fmt.Sprintf("folder %q still contains %d %s; move or delete them first", ref, count, files)
}

// folderFileCount reads the file_count the server attaches to a
// folder_not_empty refusal. JSON numbers decode as float64.
func folderFileCount(details *map[string]interface{}) (int64, bool) {
	if details == nil {
		return 0, false
	}
	raw, ok := (*details)["file_count"]
	if !ok {
		return 0, false
	}
	n, ok := raw.(float64)
	if !ok {
		return 0, false
	}
	return int64(n), true
}
