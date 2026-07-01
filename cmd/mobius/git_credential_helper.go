package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// registerGitCredentialHelperCommand attaches `mobius git-credential-helper`, a
// git credential helper (https://git-scm.com/docs/gitcredentials) backed by the
// Mobius environment git-credential broker.
//
// It lets ordinary `git` commands run from inside a managed environment (for
// example via `environment.bash`) authenticate against private GitHub repos
// without any token ever being written to disk. git invokes this helper on
// demand; the helper brokers a fresh, repository-scoped token for exactly the
// repository git names, prints it on stdout, and exits. Nothing is persisted.
//
// `environment.git.clone` wires it in by writing a global git config that points
// credential.helper at this subcommand (see configureEnvironmentGitCredentials).
// The environment id and API credentials come from the worker's environment
// (--environment / MOBIUS_WORKER_ENVIRONMENT_ID and the standard MOBIUS_API_*
// globals), so the stored config holds no secret.
func registerGitCredentialHelperCommand(app *cli.App) {
	app.Command("git-credential-helper").
		Description("Git credential helper backed by Mobius environment credentials (internal)").
		Hidden().
		Use(requireAuth()).
		Flags(
			cli.String("environment", "").
				Env("MOBIUS_WORKER_ENVIRONMENT_ID").
				Help("Managed environment ID whose credential broker mints the token"),
		).
		Args("operation").
		Run(func(ctx *cli.Context) error {
			// git calls the helper with one of get/store/erase. It only expects
			// output for get; store/erase are advisory and this helper persists
			// nothing, so they are silent no-ops.
			if ctx.Arg(0) != "get" {
				return nil
			}
			environmentID := strings.TrimSpace(ctx.String("environment"))
			if environmentID == "" {
				return fmt.Errorf("no environment id: set --environment or MOBIUS_WORKER_ENVIRONMENT_ID")
			}
			req, err := parseGitCredentialRequest(os.Stdin)
			if err != nil {
				return err
			}
			client, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			return runGitCredentialHelper(ctx.Context(), os.Stdout, client, environmentID, req)
		})
}

// gitCredentialRequest is the subset of git's credential key/value input the
// helper acts on.
type gitCredentialRequest struct {
	Protocol string
	Host     string
	Path     string
}

// parseGitCredentialRequest reads git's credential protocol from r: `key=value`
// lines terminated by a blank line or EOF.
func parseGitCredentialRequest(r io.Reader) (gitCredentialRequest, error) {
	var req gitCredentialRequest
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "protocol":
			req.Protocol = value
		case "host":
			req.Host = value
		case "path":
			req.Path = value
		}
	}
	if err := scanner.Err(); err != nil {
		return gitCredentialRequest{}, err
	}
	return req, nil
}

// gitCredentialBroker is the slice of *mobius.Client the helper needs, so the
// core logic is testable without a live client.
type gitCredentialBroker interface {
	CreateEnvironmentGitCredential(ctx context.Context, environmentID string, req mobius.EnvironmentGitCredentialRequest) (*mobius.EnvironmentGitCredential, error)
}

// runGitCredentialHelper brokers a token for the requested repo and writes it to
// out in git credential format. For a request it can't broker (non-GitHub host
// or unparseable path) it writes nothing and returns nil, so git falls back to
// its other credential sources instead of failing on this helper.
func runGitCredentialHelper(ctx context.Context, out io.Writer, broker gitCredentialBroker, environmentID string, req gitCredentialRequest) error {
	repo, ok := githubRepoFromCredentialRequest(req)
	if !ok {
		return nil
	}
	// Always request push: git's credential protocol does not distinguish a
	// fetch from a push, so the helper asks for the strongest scope and lets the
	// Mobius broker decide. The broker mints a write token only when this
	// repository was opted into the environment's push allowlist, otherwise a
	// read token — so read-only fetch/pull keep working and push is a
	// server-enforced opt-in.
	cred, err := broker.CreateEnvironmentGitCredential(ctx, environmentID, mobius.EnvironmentGitCredentialRequest{
		RepoFullName: repo,
		Operation:    "push",
	})
	if err != nil {
		return fmt.Errorf("broker git credential for %s: %w", repo, err)
	}
	return writeGitCredential(out, cred)
}

// githubRepoFromCredentialRequest derives an `owner/name` full name from a git
// credential request, or reports false when the request is not an https
// github.com remote with a usable path. Requires credential.useHttpPath=true so
// git includes the repository path.
func githubRepoFromCredentialRequest(req gitCredentialRequest) (string, bool) {
	if !strings.EqualFold(req.Host, "github.com") {
		return "", false
	}
	path := strings.TrimPrefix(strings.TrimSpace(req.Path), "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" || strings.Count(path, "/") != 1 {
		return "", false
	}
	return path, true
}

func writeGitCredential(out io.Writer, cred *mobius.EnvironmentGitCredential) error {
	if cred == nil {
		return nil
	}
	username := cred.Username
	if username == "" {
		username = "x-access-token"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "username=%s\n", username)
	fmt.Fprintf(&b, "password=%s\n", cred.Token)
	if !cred.ExpiresAt.IsZero() {
		// git honors password_expiry_utc to avoid caching a token past its life.
		fmt.Fprintf(&b, "password_expiry_utc=%s\n", strconv.FormatInt(cred.ExpiresAt.Unix(), 10))
	}
	_, err := io.WriteString(out, b.String())
	return err
}
