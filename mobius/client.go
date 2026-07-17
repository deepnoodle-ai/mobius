package mobius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
)

const defaultHTTPTimeout = 60 * time.Second
const DefaultBaseURL = "https://api.mobiusops.ai"

// DefaultMaxRetries is the default number of retry attempts made by the
// client for 429 and 503 responses. See docs/retries.md for the full
// retry policy.
const DefaultMaxRetries = 3

// Client holds connection settings for the Mobius API. Create one with NewClient
// and use it to construct Workers, start runs, or manage loops.
type Client struct {
	baseURL       string
	apiKey        string
	projectHandle string
	httpClient    *http.Client
	// transferClient serves large-body transfers (artifact upload/download).
	// http.Client.Timeout covers the entire exchange including the body, so
	// the 60s default on httpClient would abort any artifact that takes
	// longer than a minute to move; this client bounds the dial/TLS/header
	// phases on its transport instead and leaves body transfer time to the
	// caller's context.
	transferClient *http.Client
	customHTTP     bool
	maxRetries     int
	ac             *api.ClientWithResponses
	config         *ClientConfig
}

// ClientConfig holds optional client configuration.
type ClientConfig struct {
	Logger *slog.Logger
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the API key used to authenticate all requests.
// Project-pinned keys are presented to the server as
// "mbx_<secret>.<handle>" or "mbc_<secret>.<handle>" — pass the
// credential exactly as it was issued and the client will extract the
// handle for URL templating. Org-scoped keys without a suffix are also
// accepted.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

// WithBaseURL overrides the default Mobius API host.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = baseURL }
}

// WithHTTPClient replaces the default HTTP client. Useful for testing
// or for injecting custom transport (retries, tracing, etc.). When set,
// the client will not install its own retrying transport; the caller is
// responsible for retry behavior on the supplied client's Transport.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		c.httpClient = hc
		c.customHTTP = true
	}
}

// WithRetry configures how many times the built-in transport retries 429
// and 503 responses. The default is [DefaultMaxRetries]; pass 0 to disable
// retries entirely (429 responses surface as [RateLimitError] on the first
// attempt). Has no effect when a custom client is installed via
// [WithHTTPClient] — those callers manage their own retry layer.
func WithRetry(n int) Option {
	return func(c *Client) {
		if n < 0 {
			n = 0
		}
		c.maxRetries = n
	}
}

// WithLogger sets the logger used for debug output.
func WithLogger(log *slog.Logger) Option {
	return func(c *Client) { c.config.Logger = log }
}

// WithProjectHandle sets the project handle used for all project-scoped operations.
// Required for workers and project-scoped API operations.
func WithProjectHandle(handle string) Option {
	return func(c *Client) { c.projectHandle = handle }
}

// projectHandleRe matches the project-handle regex enforced by the
// server (domain/validate.go). Extracting the handle from the
// credential means the worker only needs one environment variable:
// the handle is already in the token, so passing it again via
// WithProjectHandle is redundant and will error out on conflict.
var projectHandleRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// NewClient returns a Client targeting the default Mobius API host unless
// overridden with WithBaseURL. Construction validates the credential
// shape (when WithAPIKey is supplied): a "mbx_<secret>.<handle>" or
// "mbc_<secret>.<handle>" token is split on the final dot, the handle is
// validated against the server's handle regex, and any explicit
// WithProjectHandle must match the embedded handle. All of these surface
// as an error here rather than as a 403 on the first request.
func NewClient(opts ...Option) (*Client, error) {
	c := &Client{
		baseURL:    DefaultBaseURL,
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
		maxRetries: DefaultMaxRetries,
		config: &ClientConfig{
			Logger: slog.Default(),
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	if err := c.resolveProjectHandleFromAPIKey(); err != nil {
		return nil, err
	}
	if !c.customHTTP {
		c.httpClient.Transport = &RetryingTransport{
			Base:       c.httpClient.Transport,
			MaxRetries: c.maxRetries,
			Logger:     c.config.Logger,
		}
		c.transferClient = &http.Client{
			Transport: &RetryingTransport{
				Base:       transferTransport(),
				MaxRetries: c.maxRetries,
				Logger:     c.config.Logger,
			},
		}
	} else {
		// A caller-supplied client owns its own timeout policy; use it for
		// transfers unchanged.
		c.transferClient = c.httpClient
	}
	ac, err := api.NewClientWithResponses(c.baseURL,
		api.WithHTTPClient(c.httpClient),
		api.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			if c.apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+c.apiKey)
			}
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("mobius: failed to create API client: %w", err)
	}
	c.ac = ac
	return c, nil
}

// transferTransport clones the default transport and bounds the phases that
// can hang without progress — waiting for response headers — while leaving
// body transfer unbounded (the request context still applies end to end).
func transferTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := t.Clone()
		clone.ResponseHeaderTimeout = defaultHTTPTimeout
		return clone
	}
	return http.DefaultTransport
}

// resolveProjectHandleFromAPIKey extracts the optional ".<handle>"
// suffix from the configured API key so the URL templater can stop
// requiring WithProjectHandle for project-pinned credentials. The
// apiKey itself is left untouched — the full suffixed credential still
// rides on the Authorization header, and the server
// re-runs the suffix-vs-pinned-project check as defence in depth.
func (c *Client) resolveProjectHandleFromAPIKey() error {
	if c.apiKey == "" {
		return nil
	}
	handle, ok := ProjectHandleFromAPIKey(c.apiKey)
	if !ok {
		if hasCredentialSuffix(c.apiKey) || strings.HasSuffix(c.apiKey, ".") {
			return fmt.Errorf("mobius: invalid project handle suffix in API key")
		}
		return nil
	}
	if c.projectHandle != "" && c.projectHandle != handle {
		return fmt.Errorf("mobius: WithProjectHandle(%q) conflicts with the handle embedded in the API key (%q)", c.projectHandle, handle)
	}
	c.projectHandle = handle
	return nil
}

// ProjectHandleFromAPIKey returns the project suffix from a pinned Mobius
// credential. The suffix format is mbx_<secret>.<handle> for API keys and
// mbc_<secret>.<handle> for browser-issued CLI credentials.
func ProjectHandleFromAPIKey(key string) (string, bool) {
	if !strings.HasPrefix(key, "mbx_") && !strings.HasPrefix(key, "mbc_") {
		return "", false
	}
	dot := strings.LastIndexByte(key, '.')
	if dot < 0 || dot == len(key)-1 {
		return "", false
	}
	handle := key[dot+1:]
	if !projectHandleRe.MatchString(handle) {
		return "", false
	}
	return handle, true
}

func hasCredentialSuffix(key string) bool {
	if !strings.HasPrefix(key, "mbx_") && !strings.HasPrefix(key, "mbc_") {
		return false
	}
	dot := strings.LastIndexByte(key, '.')
	return dot >= 0 && (dot != len(key)-1 || strings.HasSuffix(key, "."))
}

// RawClient returns the underlying generated ClientWithResponses for direct access
// to all generated API methods.
func (c *Client) RawClient() *api.ClientWithResponses {
	return c.ac
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, "application/json", bytes.NewReader(raw), out)
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	return c.doWithHeaders(ctx, c.httpClient, method, path, contentType, body, nil, out)
}

// doMultipartWithHeaders rides the transfer client: multipart bodies are file
// uploads whose transfer time is unbounded by design (see transferClient).
func (c *Client) doMultipartWithHeaders(ctx context.Context, method, path, contentType string, body io.Reader, headers map[string]string, out any) error {
	return c.doWithHeaders(ctx, c.transferClient, method, path, contentType, body, headers, out)
}

func (c *Client) doWithHeaders(ctx context.Context, hc *http.Client, method, path, contentType string, body io.Reader, headers map[string]string, out any) error {
	if c.projectHandle == "" {
		return fmt.Errorf("mobius: no project configured - set MOBIUS_PROJECT or pass --project")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.baseURL, "/")+path, body)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return unexpectedAPIStatus(method+" "+path, resp.StatusCode, resp.Status, resp.Header, payload)
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("mobius: decode response: %w", err)
	}
	return nil
}

// ProjectHandle returns the project handle this client is bound to,
// either from WithProjectHandle or extracted from a project-pinned API
// key. Returns "" if no handle has been resolved.
func (c *Client) ProjectHandle() string {
	return c.projectHandle
}
