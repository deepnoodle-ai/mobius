package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/internal/authstore"
	"github.com/deepnoodle-ai/mobius/mobius"
)

// authSource identifies where the active credential came from. It drives
// status reporting and lets handlers refer to the credential without
// re-deriving it from flags + env + on-disk profiles.
type authSource int

const (
	authSourceNone authSource = iota
	authSourceFlag
	authSourceEnv
	authSourceProfile
)

// resolvedAuth is the single source of truth for the credential a command
// should use. It is computed once per invocation by authMiddleware after
// flag parsing, then read by clientFromContext, the auth API helpers, and
// the auth status command.
//
// APIURL, APIKey, and Project are the *effective* values, taking the
// precedence rules into account: explicit flag/env > saved profile > built-in
// default. Each field is resolved independently.
type resolvedAuth struct {
	Source  authSource
	APIURL  string
	APIKey  string
	Project string
	Profile *authstore.Profile
}

// activeAuth caches the credential resolved for this invocation. Written by
// authMiddleware before any handler runs and read through authFor.
var (
	activeAuthMu sync.Mutex
	activeAuth   *resolvedAuth
)

// authMiddleware resolves the credential to use for this invocation and
// stashes it for later reads. It runs after flag parsing — so --profile,
// --api-key, --api-url, and --project are already populated — and before
// every handler.
//
// The middleware never fails the command: if no credential can be resolved,
// activeAuth is left with Source = authSourceNone and per-command middleware
// (requireAuth) decides whether to refuse.
func authMiddleware() cli.Middleware {
	return cli.Before(func(ctx *cli.Context) error {
		auth := resolveAuth(ctx)
		setActiveAuth(auth)

		if auth.Source == authSourceProfile {
			if warning, err := authstore.PermissionWarning(); err == nil && warning != "" {
				fmt.Fprintf(ctx.Stderr(), "mobius: warning: credentials file %s\n", warning)
			}
			if auth.Profile != nil && auth.Profile.Name != "" {
				if err := authstore.TouchProfile(auth.Profile.Name, time.Now().UTC().Format(time.RFC3339)); err != nil {
					fmt.Fprintf(ctx.Stderr(), "mobius: warning: update profile last-used: %v\n", err)
				}
			}
		}
		return nil
	})
}

// resolveAuth applies the precedence rules described on resolvedAuth. It is
// pure — no side effects, no last-used touch, no warnings. authMiddleware
// owns the side-effecting concerns so they fire exactly once per invocation.
func resolveAuth(ctx *cli.Context) *resolvedAuth {
	out := &resolvedAuth{}

	if ctx.IsSet("api-key") {
		out.APIKey = ctx.String("api-key")
		out.Source = apiKeyFlagSource(out.APIKey)
	} else if cred, err := authstore.ResolveProfile(ctx.String("profile")); err == nil && cred != nil && cred.Token != "" {
		out.Source = authSourceProfile
		out.Profile = cred
		out.APIKey = cred.RequestToken()
	}

	out.APIURL = pickURL(ctx, out.Profile)
	out.Project = pickProject(ctx, out.Profile)
	return out
}

// pickURL applies the precedence: explicit --api-url / MOBIUS_API_URL >
// saved profile > built-in default. ctx.String returns the flag's Default
// when nothing is set, so we only fall through to the default when the
// profile didn't supply one either.
func pickURL(ctx *cli.Context, p *authstore.Profile) string {
	if ctx.IsSet("api-url") {
		return ctx.String("api-url")
	}
	if p != nil && p.APIURL != "" {
		return p.APIURL
	}
	if v := ctx.String("api-url"); v != "" {
		return v
	}
	return mobius.DefaultBaseURL
}

// pickProject applies the same precedence as pickURL for the project handle.
func pickProject(ctx *cli.Context, p *authstore.Profile) string {
	if ctx.IsSet("project") {
		return ctx.String("project")
	}
	if p != nil && p.ProjectHandle != "" {
		return p.ProjectHandle
	}
	return ctx.String("project")
}

// apiKeyFlagSource decides whether an api-key flag value came from the
// command line or the MOBIUS_API_KEY environment variable. The Wonton flag
// parser doesn't expose this, so we compare against the live env value.
func apiKeyFlagSource(value string) authSource {
	if env, ok := os.LookupEnv("MOBIUS_API_KEY"); ok && env == value {
		return authSourceEnv
	}
	return authSourceFlag
}

// authFor returns the credential resolved for the running command. When
// authMiddleware has already run, the cached value is returned. Otherwise —
// for tests or other callers that bypass app.Execute — resolveAuth is invoked
// inline so consumers always get a non-nil value.
func authFor(ctx *cli.Context) *resolvedAuth {
	if a := getActiveAuth(); a != nil {
		return a
	}
	return resolveAuth(ctx)
}

func setActiveAuth(a *resolvedAuth) {
	activeAuthMu.Lock()
	defer activeAuthMu.Unlock()
	activeAuth = a
}

func getActiveAuth() *resolvedAuth {
	activeAuthMu.Lock()
	defer activeAuthMu.Unlock()
	return activeAuth
}

// requireAuth fails fast when no credential has been resolved. It replaces
// the generated `cli.RequireFlags("api-key")` so commands work whether the
// user supplied --api-key or logged in via `mobius auth login`.
func requireAuth() cli.Middleware {
	return func(next cli.Handler) cli.Handler {
		return func(ctx *cli.Context) error {
			if authFor(ctx).APIKey == "" {
				return errors.New("not authenticated. Run `mobius auth login --profile <name>` or set --api-key / MOBIUS_API_KEY")
			}
			return next(ctx)
		}
	}
}
