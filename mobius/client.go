package mobius

import (
	"context"
	"log/slog"
	"net/http"
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
// API keys are prefixed with "mbx_" and can be managed via the API or console.
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

// NewClient returns a Client targeting the default Mobius API host unless
// overridden with WithBaseURL.
func NewClient(opts ...Option) *Client {
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
		panic("mobius: failed to create API client: " + err.Error())
	}
	c.ac = ac
	return c
}

// RawClient returns the underlying generated ClientWithResponses for direct access
// to all generated API methods.
func (c *Client) RawClient() *api.ClientWithResponses {
	return c.ac
}
