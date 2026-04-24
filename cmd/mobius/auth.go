package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/tui"

	"github.com/deepnoodle-ai/mobius/internal/authstore"
	"github.com/deepnoodle-ai/mobius/mobius"
)

// deviceCodeGrantType is the RFC 8628 §3.4 grant_type for the token endpoint.
const deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// deviceCodeResponse mirrors RFC 8628 §3.2.
type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceTokenResponse is the RFC 6749 §5.1 success envelope plus the
// mobius-specific credential_id extension.
type deviceTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	CredentialID string `json:"credential_id"`
}

// oauthError matches the RFC 6749 §5.2 error envelope.
type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

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
		Args("id?").
		Run(runAuthRevoke)
}

// runAuthLogin implements RFC 8628 (OAuth 2.0 Device Authorization Grant).
// It posts form-encoded requests to /auth/device/code and /auth/device/token,
// polls until the browser-side confirm completes, then persists the returned
// bearer token locally. These two endpoints are intentionally not part of the
// typed SDK client: they use form bodies and the RFC 6749 §5.2 error envelope
// rather than the API-wide {error: {code, message}} shape.
func runAuthLogin(ctx *cli.Context) error {
	apiURL := ctx.String("api-url")
	if apiURL == "" {
		apiURL = mobius.DefaultBaseURL
	}
	apiURL = strings.TrimRight(apiURL, "/")

	httpClient := &http.Client{Timeout: 15 * time.Second}

	label := ctx.String("label")
	if label == "" {
		label = defaultDeviceLabel()
	}

	form := url.Values{}
	if label != "" {
		form.Set("mobius_label", label)
	}
	if org := ctx.String("org"); org != "" {
		form.Set("mobius_requested_org_id", org)
	}

	challenge, err := postDeviceCode(ctx.Context(), httpClient, apiURL, form)
	if err != nil {
		return err
	}

	ctx.Println("")
	ctx.Printf("  Your verification code: %s\n", challenge.UserCode)
	ctx.Printf("  Open this URL:          %s\n", challenge.VerificationURIComplete)
	ctx.Println("")

	if !ctx.Bool("no-browser") {
		if err := openBrowser(challenge.VerificationURIComplete); err != nil {
			ctx.Warn("could not open browser automatically: %s", err)
		}
	}

	ctx.Println("Waiting for you to confirm in the browser...")

	token, credID, err := pollForToken(ctx.Context(), httpClient, apiURL, challenge)
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

// postDeviceCode calls RFC 8628 §3.1 (Device Authorization Request) with a
// form body and decodes the flat §3.2 JSON response.
func postDeviceCode(ctx context.Context, client *http.Client, apiURL string, form url.Values) (*deviceCodeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/v1/auth/device/code", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var oe oauthError
		if err := json.Unmarshal(body, &oe); err == nil && oe.Error != "" {
			if oe.ErrorDescription != "" {
				return nil, fmt.Errorf("request device code: %s (%s)", oe.Error, oe.ErrorDescription)
			}
			return nil, fmt.Errorf("request device code: %s", oe.Error)
		}
		return nil, fmt.Errorf("request device code: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out deviceCodeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode device code response: %w", err)
	}
	return &out, nil
}

// pollForToken exchanges the device code at the server-requested cadence
// using RFC 8628 §3.4–3.5 semantics: authorization_pending and slow_down
// responses are expected during the normal waiting window, everything else
// is terminal. We honor the server's suggested interval, increase it by 5s on
// slow_down, and cap the overall wait by expires_in.
func pollForToken(ctx context.Context, client *http.Client, apiURL string, ch *deviceCodeResponse) (string, string, error) {
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

		form := url.Values{
			"grant_type":  {deviceCodeGrantType},
			"device_code": {ch.DeviceCode},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/v1/auth/device/token", strings.NewReader(form.Encode()))
		if err != nil {
			return "", "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			return "", "", fmt.Errorf("poll for token: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var tok deviceTokenResponse
			if err := json.Unmarshal(body, &tok); err != nil {
				return "", "", fmt.Errorf("decode token response: %w", err)
			}
			if tok.AccessToken == "" {
				return "", "", errors.New("server returned 200 but no access_token")
			}
			return tok.AccessToken, tok.CredentialID, nil
		}

		var oe oauthError
		if err := json.Unmarshal(body, &oe); err != nil {
			return "", "", fmt.Errorf("poll for token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		switch oe.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied":
			return "", "", errors.New("login was denied in the browser")
		case "expired_token":
			return "", "", errors.New("login flow expired before confirmation; run `mobius auth login` again")
		default:
			if oe.ErrorDescription != "" {
				return "", "", fmt.Errorf("device token exchange failed: %s (%s)", oe.Error, oe.ErrorDescription)
			}
			return "", "", fmt.Errorf("device token exchange failed: %s", oe.Error)
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

	if appliedSavedCredential != nil && ctx.String("api-key") == appliedSavedCredential.Token {
		printSavedCredentialStatus(ctx, appliedSavedCredential)
		return nil
	}

	if ctx.IsSet("api-key") {
		// Distinguish between flag and env var as best we can.
		if _, ok := os.LookupEnv("MOBIUS_API_KEY"); ok && os.Getenv("MOBIUS_API_KEY") == ctx.String("api-key") {
			ctx.Println("Auth source: MOBIUS_API_KEY environment variable")
		} else {
			ctx.Println("Auth source: --api-key flag")
		}
		printAuthVerification(ctx, "raw API key")
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
	printSavedCredentialStatus(ctx, cred)
	return nil
}

func printSavedCredentialStatus(ctx *cli.Context, cred *authstore.Credential) {
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
	printAuthVerification(ctx, "browser-based CLI credential")
}

func printAuthVerification(ctx *cli.Context, credentialKind string) {
	status, err := verifyAuthenticatedRequest(ctx)
	if err != nil {
		ctx.Printf("Auth check: GET /v1/projects failed: %v\n", err)
		ctx.Printf("Authenticated: no (%s verification failed)\n", credentialKind)
		return
	}
	ctx.Printf("Auth check: GET /v1/projects -> HTTP %d\n", status)
	if status == http.StatusOK {
		ctx.Printf("Authenticated: yes (%s verified)\n", credentialKind)
		return
	}
	ctx.Printf("Authenticated: no (%s rejected)\n", credentialKind)
}

type cliCredential struct {
	Id         string     `json:"id"`
	Status     string     `json:"status"`
	Label      string     `json:"label"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type cliCredentialListResponse struct {
	Items []cliCredential `json:"items"`
}

func fetchCLICredentials(ctx *cli.Context) ([]cliCredential, error) {
	resp, err := authAPIGet(ctx, "/v1/auth/cli-credentials")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list CLI credentials: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var result cliCredentialListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("list CLI credentials: decode: %w", err)
	}
	return result.Items, nil
}

func printCLICredentials(ctx *cli.Context, creds []cliCredential) error {
	rows := make([][]string, 0, len(creds))
	for _, c := range creds {
		lastUsed := "never"
		if c.LastUsedAt != nil {
			lastUsed = c.LastUsedAt.Format(time.RFC3339)
		}
		rows = append(rows, []string{c.Id, c.Status, c.Label, lastUsed})
	}
	// Non-interactive print: -1 means "no row selected" so the table
	// doesn't reverse-video the first row.
	sel := -1
	view := tui.Table([]tui.TableColumn{
		{Title: "ID"},
		{Title: "STATUS"},
		{Title: "LABEL"},
		{Title: "LAST USED"},
	}, &sel).Rows(rows)
	if err := tui.Fprint(ctx.Stdout(), view); err != nil {
		return err
	}
	ctx.Println("")
	return nil
}

// runAuthList lists the CLI credentials the authenticated user has minted
// across devices. This hits an authenticated endpoint, so it uses the
// resolved client (saved credential or explicit key).
func runAuthList(ctx *cli.Context) error {
	if !hasAuth(ctx) {
		return errLoginRequired()
	}
	creds, err := fetchCLICredentials(ctx)
	if err != nil {
		return err
	}
	if len(creds) == 0 {
		ctx.Println("No CLI credentials.")
		return nil
	}
	return printCLICredentials(ctx, creds)
}

func runAuthRevoke(ctx *cli.Context) error {
	if !hasAuth(ctx) {
		return errLoginRequired()
	}
	id := ctx.Arg(0)
	if id == "" {
		creds, err := fetchCLICredentials(ctx)
		if err != nil {
			return err
		}
		if len(creds) == 0 {
			ctx.Println("No CLI credentials to revoke.")
			return nil
		}
		ctx.Println("Specify a credential ID to revoke. Available credentials:")
		if err := printCLICredentials(ctx, creds); err != nil {
			return err
		}
		return errors.New("specify a credential ID to revoke: `mobius auth revoke <id>`")
	}
	req, err := http.NewRequestWithContext(ctx.Context(), http.MethodDelete, authAPIURL(ctx, "/v1/auth/cli-credentials/"+id), nil)
	if err != nil {
		return err
	}
	authAPISetHeaders(ctx, req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("revoke CLI credential: HTTP %d: %s", resp.StatusCode, string(body))
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

func authAPIURL(ctx *cli.Context, path string) string {
	base := strings.TrimRight(ctx.String("api-url"), "/")
	return base + path
}

func authAPISetHeaders(ctx *cli.Context, req *http.Request) {
	key := ctx.String("api-key")
	if key == "" {
		if cred, err := authstore.Load(); err == nil && cred != nil {
			key = cred.Token
		}
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func authAPIGet(ctx *cli.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx.Context(), http.MethodGet, authAPIURL(ctx, path), nil)
	if err != nil {
		return nil, err
	}
	authAPISetHeaders(ctx, req)
	return http.DefaultClient.Do(req)
}

func verifyAuthenticatedRequest(ctx *cli.Context) (int, error) {
	resp, err := authAPIGet(ctx, "/v1/projects")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
