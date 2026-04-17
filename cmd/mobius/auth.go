package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/deepnoodle-ai/wonton/cli"

	"github.com/deepnoodle-ai/mobius/internal/authstore"
	"github.com/deepnoodle-ai/mobius/mobius"
	"github.com/deepnoodle-ai/mobius/mobius/api"
)

// registerAuthCommands wires up the `mobius auth` group. Unlike most groups,
// auth is entirely hand-written: the generated commands for the device-flow
// endpoints are suppressed in internal/cligen/overrides.go because they are
// only useful when driven together, not as individual raw CLI calls.
func registerAuthCommands(app *cli.App) {
	grp := app.Group("auth").Description("Browser-based CLI authentication")

	grp.Command("login").
		Description("Log this device into Mobius via the browser").
		Flags(
			cli.String("org", "").Help("Requested org ID (optional; browser will prompt otherwise)"),
			cli.String("label", "").Help("Device label shown in the web app (defaults to user@host)"),
			cli.Bool("no-browser", "").Default(false).Help("Do not try to open the browser automatically"),
		).
		Run(runAuthLogin)

	grp.Command("logout").
		Description("Remove the saved browser-based CLI credential").
		Run(runAuthLogout)

	grp.Command("status").
		Description("Show the current CLI authentication status").
		Run(runAuthStatus)

	grp.Command("list").
		Description("List browser-issued CLI credentials for the current user").
		Run(runAuthList)

	grp.Command("revoke").
		Description("Revoke one browser-issued CLI credential").
		Args("id").
		Run(runAuthRevoke)
}

// runAuthLogin implements the OAuth-device-flow-style login. It builds an
// unauthenticated client (the two device endpoints are public), creates a
// challenge, opens the browser, polls until the user confirms, then persists
// the returned token locally.
func runAuthLogin(ctx *cli.Context) error {
	apiURL := ctx.String("api-url")
	if apiURL == "" {
		apiURL = mobius.DefaultBaseURL
	}

	// Use an unauthenticated client here: the login endpoints are public and
	// we do not want a stale --api-key to be sent on the challenge POST.
	unauth := mobius.NewClient(
		mobius.WithBaseURL(apiURL),
		mobius.WithLogger(newLogger(ctx.String("log-level"))),
	).RawClient()

	label := ctx.String("label")
	if label == "" {
		label = defaultDeviceLabel()
	}

	req := api.CreateDeviceCodeJSONRequestBody{
		Data: api.CreateDeviceCodeRequest{Label: &label},
	}
	if org := ctx.String("org"); org != "" {
		req.Data.RequestedOrgId = &org
	}

	createResp, err := unauth.CreateDeviceCodeWithResponse(ctx.Context(), req)
	if err != nil {
		return fmt.Errorf("request device code: %w", err)
	}
	if createResp.JSON200 == nil {
		return fmt.Errorf("request device code: HTTP %d: %s", createResp.StatusCode(), string(createResp.Body))
	}
	challenge := createResp.JSON200

	ctx.Println("")
	ctx.Printf("  Your verification code: %s\n", challenge.UserCode)
	ctx.Printf("  Open this URL:          %s\n", challenge.VerificationUriComplete)
	ctx.Println("")

	if !ctx.Bool("no-browser") {
		if err := openBrowser(challenge.VerificationUriComplete); err != nil {
			ctx.Warn("could not open browser automatically: %s", err)
		}
	}

	ctx.Println("Waiting for you to confirm in the browser...")

	token, credID, err := pollForToken(ctx.Context(), unauth, challenge)
	if err != nil {
		return err
	}

	cred := &authstore.Credential{
		Source:       authstore.SourceBrowserLogin,
		APIURL:       apiURL,
		Token:        token,
		CredentialID: credID,
		OrgID:        ctx.String("org"),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := authstore.Save(cred); err != nil {
		return fmt.Errorf("save credential: %w", err)
	}

	path, _ := authstore.Path()
	ctx.Success("Logged in. Credential saved to %s", path)
	return nil
}

// pollForToken exchanges the device code at the server-requested cadence,
// stopping when the flow completes, is denied, or expires. We honor the
// server's suggested interval and cap the overall wait by expires_in so a
// stuck browser does not hang the CLI forever.
func pollForToken(ctx context.Context, client *api.ClientWithResponses, ch *api.DeviceCodeResponse) (string, string, error) {
	interval := time.Duration(ch.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresAt := time.Now().Add(time.Duration(ch.ExpiresIn) * time.Second)

	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(expiresAt) {
			return "", "", errors.New("login flow expired before confirmation; run `mobius auth login` again")
		}
		resp, err := client.ExchangeDeviceCodeWithResponse(ctx, api.ExchangeDeviceCodeJSONRequestBody{
			Data: api.ExchangeDeviceCodeRequest{DeviceCode: ch.DeviceCode},
		})
		if err != nil {
			return "", "", fmt.Errorf("poll for token: %w", err)
		}
		if resp.JSON200 == nil {
			return "", "", fmt.Errorf("poll for token: HTTP %d: %s", resp.StatusCode(), string(resp.Body))
		}
		switch string(resp.JSON200.Status) {
		case "authorization_pending":
			continue
		case "complete":
			if resp.JSON200.Token == nil || *resp.JSON200.Token == "" {
				return "", "", errors.New("server returned complete status but no token")
			}
			credID := ""
			if resp.JSON200.CredentialId != nil {
				credID = *resp.JSON200.CredentialId
			}
			return *resp.JSON200.Token, credID, nil
		case "denied":
			return "", "", errors.New("login was denied in the browser")
		case "expired":
			return "", "", errors.New("login flow expired before confirmation; run `mobius auth login` again")
		default:
			return "", "", fmt.Errorf("unexpected device token status: %q", resp.JSON200.Status)
		}
	}
}

// runAuthLogout removes the local credential file. We intentionally do not
// revoke the remote credential here in v1 — open question in the PRD — so
// the user can still `mobius auth list` and revoke specific devices
// explicitly if they want.
func runAuthLogout(ctx *cli.Context) error {
	cred, err := authstore.Load()
	if err != nil {
		return err
	}
	if cred == nil {
		ctx.Println("No saved credential to remove.")
		return nil
	}
	if err := authstore.Delete(); err != nil {
		return err
	}
	ctx.Success("Removed saved credential for %s", cred.APIURL)
	return nil
}

// runAuthStatus prints which credential source the next CLI call would use.
// The precedence here mirrors clientFromContext: --api-key, then
// MOBIUS_API_KEY, then the saved browser credential.
func runAuthStatus(ctx *cli.Context) error {
	ctx.Printf("API URL: %s\n", ctx.String("api-url"))

	if ctx.IsSet("api-key") {
		// Distinguish between flag and env var as best we can.
		if _, ok := os.LookupEnv("MOBIUS_API_KEY"); ok && os.Getenv("MOBIUS_API_KEY") == ctx.String("api-key") {
			ctx.Println("Auth source: MOBIUS_API_KEY environment variable")
		} else {
			ctx.Println("Auth source: --api-key flag")
		}
		ctx.Println("Authenticated: yes (raw API key)")
		return nil
	}

	cred, err := authstore.Load()
	if err != nil {
		return err
	}
	if cred == nil || cred.Token == "" {
		ctx.Println("Auth source: none")
		ctx.Println("Authenticated: no")
		ctx.Println("Run `mobius auth login` to sign in from the browser.")
		return nil
	}
	ctx.Println("Auth source: saved browser credential")
	ctx.Printf("Credential file: ")
	if p, err := authstore.Path(); err == nil {
		ctx.Println(p)
	} else {
		ctx.Println("(unknown)")
	}
	if cred.CredentialID != "" {
		ctx.Printf("Credential ID: %s\n", cred.CredentialID)
	}
	if cred.OrgName != "" || cred.OrgID != "" {
		ctx.Printf("Org: %s\n", firstNonEmpty(cred.OrgName, cred.OrgID))
	}
	if cred.UserEmail != "" || cred.UserName != "" || cred.UserID != "" {
		ctx.Printf("User: %s\n", firstNonEmpty(cred.UserEmail, cred.UserName, cred.UserID))
	}
	ctx.Println("Authenticated: yes (browser-based CLI credential)")
	return nil
}

// runAuthList lists the CLI credentials the authenticated user has minted
// across devices. This hits an authenticated endpoint, so it uses the
// resolved client (saved credential or explicit key).
func runAuthList(ctx *cli.Context) error {
	if !hasAuth(ctx) {
		return errLoginRequired()
	}
	client := clientFromContext(ctx).RawClient()
	resp, err := client.ListCLICredentialsWithResponse(ctx.Context())
	if err != nil {
		return err
	}
	if resp.JSON200 == nil {
		return fmt.Errorf("list CLI credentials: HTTP %d: %s", resp.StatusCode(), string(resp.Body))
	}
	if len(resp.JSON200.Data) == 0 {
		ctx.Println("No CLI credentials.")
		return nil
	}
	ctx.Printf("%-28s  %-8s  %-32s  %s\n", "ID", "STATUS", "LABEL", "LAST USED")
	for _, c := range resp.JSON200.Data {
		lastUsed := "never"
		if c.LastUsedAt != nil {
			lastUsed = c.LastUsedAt.Format(time.RFC3339)
		}
		ctx.Printf("%-28s  %-8s  %-32s  %s\n", c.Id, c.Status, truncate(c.Label, 32), lastUsed)
	}
	return nil
}

func runAuthRevoke(ctx *cli.Context) error {
	if !hasAuth(ctx) {
		return errLoginRequired()
	}
	id := ctx.Arg(0)
	client := clientFromContext(ctx).RawClient()
	resp, err := client.RevokeCLICredentialWithResponse(ctx.Context(), id)
	if err != nil {
		return err
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return fmt.Errorf("revoke CLI credential: HTTP %d: %s", resp.StatusCode(), string(resp.Body))
	}
	ctx.Success("Revoked CLI credential %s", id)
	// If the user revoked the credential they are currently using, the
	// saved copy is now useless — wipe it so the next command fails fast
	// with a clear "not logged in" rather than a 401.
	if cred, err := authstore.Load(); err == nil && cred != nil && cred.CredentialID == id {
		_ = authstore.Delete()
	}
	return nil
}

// hasAuth reports whether clientFromContext will have a usable token.
func hasAuth(ctx *cli.Context) bool {
	if ctx.IsSet("api-key") {
		return true
	}
	cred, err := authstore.Load()
	if err != nil {
		return false
	}
	return cred != nil && cred.Token != ""
}

func errLoginRequired() error {
	return errors.New("not authenticated. Run `mobius auth login` or set --api-key / MOBIUS_API_KEY")
}

// openBrowser best-effort opens the URL in the user's browser. We return the
// error so the caller can fall back to "visit this URL manually".
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func defaultDeviceLabel() string {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	host, _ := os.Hostname()
	switch {
	case user != "" && host != "":
		return fmt.Sprintf("mobius CLI on %s@%s", user, host)
	case host != "":
		return fmt.Sprintf("mobius CLI on %s", host)
	default:
		return "mobius CLI"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
