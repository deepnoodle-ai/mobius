package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/internal/authstore"
	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerAgentsExtras layers hand-written commands on top of the generated
// `agents` group:
//
//   - `create` (replaces the generated form): adds --install-credentials and
//     --profile-name so a fresh agent can be provisioned with its own
//     credential profile in one CLI call (PRD 048 §2.2).
//   - `issue-key`: mints an API key for an existing agent's service account,
//     either installing it as a profile or printing it for off-machine use.
func registerAgentsExtras(app *cli.App) {
	grp := app.Group("agents")

	grp.Command("create").
		Description("Create an agent (optionally minting and installing a credential profile)").
		Flags(
			cli.String("capabilities", "").Help("Arbitrary capability map used by orchestrators to select suitable agents. Accepts JSON, @file, or @-."),
			cli.String("config", "").Help("Agent-specific configuration stored and returned opaquely. Accepts JSON, @file, or @-."),
			cli.String("description", "").Help("Optional human-readable description."),
			cli.String("kind", "").Help("Freeform classification (e.g. \"llm\", \"rpa\", \"integration\")."),
			cli.String("name", "").Help("[required] Project-scoped unique name for this agent. 1-63 characters."),
			cli.String("service-account-id", "").Help("Service account that backs this agent. Must be active and belong to the same project. If omitted, a new SA is auto-created."),
			cli.Strings("tag", "").Help("Tag in KEY=VALUE form. Repeatable."),
			cli.String("file", "f").Help("Request body from a file (JSON or YAML, '-' for stdin). Flags override file contents."),
			cli.Bool("dry-run", "").Help("Print the assembled request body and exit without sending it."),
			cli.Bool("install-credentials", "").Help("After creation, mint an API key for the agent's service account and save it as a local profile in ~/.mobius/credentials."),
			cli.String("profile-name", "").Help("Profile name to write when --install-credentials is set. Defaults to the slugified agent name."),
		).
		Use(requireAuth()).
		Run(agentsCreateHandler)

	grp.Command("issue-key").
		Description("Mint an API key for an existing agent's service account.").
		Args("agent").
		Flags(
			cli.Bool("install-credentials", "").Default(true).Help("Save the minted key as a profile in ~/.mobius/credentials."),
			cli.String("profile-name", "").Help("Profile name to write. Defaults to the slugified agent name."),
			cli.Bool("print", "").Help("Print the raw key on stdout instead of saving a profile (mutually exclusive with --install-credentials)."),
			cli.String("key-name", "").Help("Human-readable label stored on the API key (defaults to '<agent-name>-cli')."),
			cli.String("expires-at", "").Help("Optional RFC3339 expiry (e.g. 2027-01-01T00:00:00Z). Omit for a non-expiring key."),
		).
		Use(requireAuth()).
		Run(agentsIssueKeyHandler)
}

func agentsCreateHandler(ctx *cli.Context) error {
	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	project := authFor(ctx).Project

	var body api.CreateAgentJSONRequestBody
	if err := readJSONBody(ctx, &body); err != nil {
		return err
	}
	if ctx.IsSet("capabilities") {
		if err := decodeFlagJSON(ctx, "capabilities", ctx.String("capabilities"), &body.Capabilities); err != nil {
			return err
		}
	}
	if ctx.IsSet("config") {
		if err := decodeFlagJSON(ctx, "config", ctx.String("config"), &body.Config); err != nil {
			return err
		}
	}
	if ctx.IsSet("description") {
		v := ctx.String("description")
		body.Description = &v
	}
	if ctx.IsSet("kind") {
		v := ctx.String("kind")
		body.Kind = &v
	}
	if ctx.IsSet("name") {
		body.Name = ctx.String("name")
	}
	if ctx.IsSet("service-account-id") {
		v := ctx.String("service-account-id")
		body.ServiceAccountId = &v
	}
	if tags, err := parseTagFlags(ctx); err != nil {
		return err
	} else if tags != nil {
		v := api.TagMap(tags)
		body.Tags = &v
	}
	if body.Name == "" {
		return cli.Errorf("--name is required (or supply it via --file)")
	}

	install := ctx.Bool("install-credentials")
	profileName := strings.TrimSpace(ctx.String("profile-name"))

	// Validate the profile target before doing the side-effecting create —
	// the PRD calls for hard-fail on collision rather than silent overwrite,
	// and discovering the collision after the agent exists strands a fresh
	// SA without a credential.
	if install {
		if profileName == "" {
			profileName = slugifyProfileName(body.Name)
		}
		if err := assertProfileNameAvailable(profileName); err != nil {
			return err
		}
	}

	if ctx.Bool("dry-run") {
		return printDryRun(ctx, body)
	}

	resp, err := client.CreateAgentWithResponse(ctx.Context(), project, body)
	if err != nil {
		return err
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 || resp.JSON201 == nil {
		// Surface the server's error verbatim through the standard renderer.
		return printResponse(ctx, "createAgent", resp.StatusCode(), resp.Body)
	}

	if err := printResponse(ctx, "createAgent", resp.StatusCode(), resp.Body); err != nil {
		return err
	}
	if !install {
		return nil
	}

	agent := resp.JSON201
	return mintAndInstallAgentKey(ctx, agent, mintOptions{
		ProfileName: profileName,
		KeyName:     agent.Name + "-cli",
	})
}

func agentsIssueKeyHandler(ctx *cli.Context) error {
	print := ctx.Bool("print")
	install := ctx.Bool("install-credentials")
	if print && install && ctx.IsSet("install-credentials") {
		return cli.Errorf("--install-credentials and --print are mutually exclusive")
	}
	// --print on its own implies "do not install".
	if print {
		install = false
	}

	mc, err := clientFromContext(ctx)
	if err != nil {
		return err
	}
	client := mc.RawClient()
	project := authFor(ctx).Project

	agentRef := ctx.Arg(0)
	if agentRef == "" {
		return cli.Errorf("agent id is required: `mobius agents issue-key <agent>`")
	}

	agent, err := resolveAgent(ctx, client, project, agentRef)
	if err != nil {
		return err
	}

	profileName := strings.TrimSpace(ctx.String("profile-name"))
	if install {
		if profileName == "" {
			profileName = slugifyProfileName(agent.Name)
		}
		if err := assertProfileNameAvailable(profileName); err != nil {
			return err
		}
	}

	keyName := strings.TrimSpace(ctx.String("key-name"))
	if keyName == "" {
		keyName = agent.Name + "-cli"
	}

	opts := mintOptions{
		KeyName:   keyName,
		ExpiresAt: strings.TrimSpace(ctx.String("expires-at")),
	}
	if install {
		opts.ProfileName = profileName
	} else {
		opts.PrintToStdout = true
	}
	return mintAndInstallAgentKey(ctx, agent, opts)
}

// resolveAgent looks up an agent by ID. We accept only IDs (not names) here
// because name resolution would require a list call and the PRD's agent loop
// example consistently uses IDs from `mobius agents create` output.
func resolveAgent(ctx *cli.Context, client api.ClientWithResponsesInterface, project, ref string) (*api.Agent, error) {
	resp, err := client.GetAgentWithResponse(ctx.Context(), project, ref)
	if err != nil {
		return nil, fmt.Errorf("get agent %q: %w", ref, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		return nil, &cli.ExitError{Code: exitCodeForStatus(http.StatusNotFound), Message: fmt.Sprintf("agent %q not found in project %q", ref, project)}
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 || resp.JSON200 == nil {
		return nil, &cli.ExitError{Code: exitCodeForStatus(resp.StatusCode()), Message: fmt.Sprintf("get agent %q: HTTP %d: %s", ref, resp.StatusCode(), strings.TrimSpace(string(resp.Body)))}
	}
	return resp.JSON200, nil
}

type mintOptions struct {
	KeyName       string
	ProfileName   string
	ExpiresAt     string
	PrintToStdout bool
}

// apiKeyCreateRequest is the request shape for POST /v1/api-keys. The
// api-keys endpoint is intentionally absent from the public OpenAPI spec
// (filtered out by scripts/openapi-filter-public.py), so we hand-shape the
// payload here rather than route it through the typed SDK client.
type apiKeyCreateRequest struct {
	Name             string `json:"name"`
	ServiceAccountID string `json:"service_account_id"`
	ExpiresAt        string `json:"expires_at,omitempty"`
}

type apiKeyCreateResponse struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	KeyPrefix        string `json:"key_prefix"`
	Scope            string `json:"scope"`
	Key              string `json:"key"`
	ServiceAccountID string `json:"service_account_id"`
	ProjectID        string `json:"project_id"`
}

// mintAndInstallAgentKey is the shared body of `agents create
// --install-credentials` and `agents issue-key`. It posts to the API-keys
// endpoint binding the new key to `agent.ServiceAccountId`, then either
// writes a local credential profile or prints the raw token, depending on
// `opts`.
func mintAndInstallAgentKey(ctx *cli.Context, agent *api.Agent, opts mintOptions) error {
	auth := authFor(ctx)

	if opts.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, opts.ExpiresAt); err != nil {
			return cli.Errorf("--expires-at %q: expected RFC3339 (e.g. 2027-01-01T00:00:00Z)", opts.ExpiresAt)
		}
	}

	created, err := createAgentAPIKey(ctx.Context(), auth, apiKeyCreateRequest{
		Name:             opts.KeyName,
		ServiceAccountID: agent.ServiceAccountId,
		ExpiresAt:        opts.ExpiresAt,
	})
	if err != nil {
		return err
	}

	if opts.PrintToStdout {
		ctx.Println(created.Key)
		return nil
	}
	if opts.ProfileName == "" {
		// Defensive: callers should set ProfileName or PrintToStdout.
		ctx.Println(created.Key)
		return nil
	}

	profile := authstore.Profile{
		Source:           authstore.SourceAgentInstall,
		APIURL:           auth.APIURL,
		Token:            created.Key,
		CredentialID:     created.ID,
		ProjectHandle:    auth.Project,
		ProjectID:        created.ProjectID,
		ServiceAccountID: agent.ServiceAccountId,
		AgentID:          agent.Id,
		AgentName:        agent.Name,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	if auth.Profile != nil {
		profile.OrgID = auth.Profile.OrgID
		profile.OrgName = auth.Profile.OrgName
	}
	if err := authstore.PutProfile(opts.ProfileName, profile, false); err != nil {
		return fmt.Errorf("save profile: %w", err)
	}
	path, _ := authstore.Path()
	ctx.Success("Installed agent profile %q in %s", opts.ProfileName, path)
	ctx.Printf("\nUse this profile from a sub-process:\n  MOBIUS_PROFILE=%s mobius agents list\n", opts.ProfileName)
	return nil
}

// createAgentAPIKey issues a project-pinned API key for the agent's SA. We
// pass `?project_id=<handle>` so the server validates SA ownership and the
// returned token comes back with the project-handle suffix already applied
// (see api_keys.go in mobius-cloud).
func createAgentAPIKey(ctx context.Context, auth *resolvedAuth, req apiKeyCreateRequest) (*apiKeyCreateResponse, error) {
	if auth.APIURL == "" {
		return nil, fmt.Errorf("no API URL resolved; run `mobius auth login`")
	}
	if auth.APIKey == "" {
		return nil, fmt.Errorf("not authenticated; run `mobius auth login`")
	}

	bodyJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode api-key request: %w", err)
	}

	endpoint := strings.TrimRight(auth.APIURL, "/") + "/v1/api-keys"
	if auth.Project != "" {
		endpoint += "?" + url.Values{"project_id": []string{auth.Project}}.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+auth.APIKey)

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create api key: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	respBody, _ := io.ReadAll(httpResp.Body)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, &cli.ExitError{
			Code:    exitCodeForStatus(httpResp.StatusCode),
			Message: fmt.Sprintf("create api key: HTTP %d: %s", httpResp.StatusCode, strings.TrimSpace(string(respBody))),
		}
	}
	var out apiKeyCreateResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode api-key response: %w", err)
	}
	if out.Key == "" {
		return nil, fmt.Errorf("server returned 2xx but no key in response")
	}
	return &out, nil
}

// assertProfileNameAvailable refuses to overwrite an existing profile. The
// PRD calls for hard-fail with a message that points at the recovery paths
// rather than auto-suffixing or silently replacing.
func assertProfileNameAvailable(name string) error {
	store, err := authstore.LoadStore()
	if err != nil {
		return err
	}
	if _, exists := store.Profiles[name]; !exists {
		return nil
	}
	path, _ := authstore.Path()
	return cli.Errorf(
		"profile %q already exists in %s.\n"+
			"       Run `mobius auth list` to see existing profiles.\n"+
			"       Pass --profile-name <name> to use a different profile name,\n"+
			"       or `mobius auth remove %s` to remove the existing one first.",
		name, path, name,
	)
}

// slugifyProfileName lower-cases an agent name and replaces any character
// outside [a-z0-9_-] with `-`. Multiple separators collapse to one and
// leading/trailing separators are trimmed. Used as the default profile name
// when `--profile-name` is not passed.
func slugifyProfileName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	prevDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == ' ' || r == '/' || r == '.':
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		default:
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "agent"
	}
	return out
}
