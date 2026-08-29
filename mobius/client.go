package mobius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	baseURL    string
	apiKey     string
	httpClient *http.Client
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

// WithAPIKey sets the API key used to authenticate all requests. The key
// determines the org every request is scoped to; pass the credential
// exactly as it was issued.
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

// NewClient returns a Client targeting the default Mobius API host unless
// overridden with WithBaseURL.
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
