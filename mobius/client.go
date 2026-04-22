package mobius

import (
	"context"
	"fmt"
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
// and use it to construct Workers, start runs, or manage workflows.
type Client struct {
	baseURL       string
	apiKey        string
	projectHandle string
	httpClient    *http.Client
	customHTTP    bool
	maxRetries    int
	ac            *api.ClientWithResponses
	config        *ClientConfig
}

// ClientConfig holds optional client configuration.
type ClientConfig struct {
	Logger *slog.Logger
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the API key used to authenticate all requests.
// Project-pinned keys are presented to the server as
// "<handle>/mbx_<secret>" — pass the credential exactly as it was
// issued and the client will extract the handle for URL templating,
// so a single credential is sufficient to configure a worker. Org-
// scoped keys ("mbx_<secret>" with no prefix) are also accepted.
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
// shape (when WithAPIKey is supplied): a "<handle>/mbx_<secret>" token
// is split on the first slash, the handle is validated against the
// server's handle regex, and any explicit WithProjectHandle must match
// the embedded handle. All of these surface as an error here rather
// than as a 403 on the first request.
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

// resolveProjectHandleFromAPIKey extracts the optional "<handle>/"
// prefix from the configured API key so the URL templater can stop
// requiring WithProjectHandle for project-pinned credentials. The
// apiKey itself is left untouched — the full "<handle>/mbx_<secret>"
// string still rides on the Authorization header, and the server
// re-runs the prefix-vs-pinned-project check as defence in depth.
func (c *Client) resolveProjectHandleFromAPIKey() error {
	if c.apiKey == "" {
		return nil
	}
	slash := strings.IndexByte(c.apiKey, '/')
	if slash < 0 {
		return nil
	}
	handle := c.apiKey[:slash]
	if !projectHandleRe.MatchString(handle) {
		return fmt.Errorf("mobius: invalid project handle prefix in API key: %q", handle)
	}
	if c.projectHandle != "" && c.projectHandle != handle {
		return fmt.Errorf("mobius: WithProjectHandle(%q) conflicts with the handle embedded in the API key (%q)", c.projectHandle, handle)
	}
	c.projectHandle = handle
	return nil
}

// RawClient returns the underlying generated ClientWithResponses for direct access
// to all generated API methods.
func (c *Client) RawClient() *api.ClientWithResponses {
	return c.ac
}
