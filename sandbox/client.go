// Package sandbox is the Go SDK for the sandbox service. It talks to the
// sbx-api service, which bridges to the Substrate
// control plane and router; `sbx deploy` runs that service
// in-cluster.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agent-substrate/sandbox/internal/guest"
	"github.com/agent-substrate/sandbox/internal/service"
)

// ErrNotFound is returned when a sandbox, file, or directory does not exist.
var ErrNotFound = errors.New("not found")

// ClientOptions configures a Client.
type ClientOptions struct {
	// Endpoint is the base URL of the sbx-api service, e.g.
	// "http://localhost:7777". Required.
	Endpoint string

	// HTTPClient overrides the http.Client used for API requests.
	HTTPClient *http.Client
}

// Client manages sandboxes against a sbx-api endpoint over HTTP.
type Client struct {
	endpoint string
	opts     ClientOptions
	http     *http.Client
}

// NewClient returns a Client targeting endpoint.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("sandbox: ClientOptions.Endpoint is required")
	}
	endpoint := strings.TrimRight(opts.Endpoint, "/")
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		endpoint: endpoint,
		opts:     opts,
		http:     httpClient,
	}, nil
}

// Close releases the client's resources.
func (c *Client) Close() error { return nil }

// CreateOption customizes Create.
type CreateOption func(*createConfig)

type createConfig struct {
	template          string
	templateNamespace string
}

// WithTemplate overrides the client's default ActorTemplate name.
func WithTemplate(name string) CreateOption {
	return func(c *createConfig) { c.template = name }
}

// WithNamespace overrides the Kubernetes namespace the ActorTemplate is
// looked up in.
func WithNamespace(namespace string) CreateOption {
	return func(c *createConfig) { c.templateNamespace = namespace }
}

// Create registers a new sandbox with the given ID (a DNS-1123 label) and
// starts it.
func (c *Client) Create(ctx context.Context, id string, opts ...CreateOption) (*Sandbox, error) {
	var cfg createConfig
	for _, o := range opts {
		o(&cfg)
	}
	template := cfg.template
	namespace := cfg.templateNamespace
	req := service.CreateSandboxRequest{
		ID:        id,
		Template:  template,
		Namespace: namespace,
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes", req, nil); err != nil {
		return nil, err
	}
	return &Sandbox{id: id, client: c}, nil
}

// Sandbox returns a handle to a sandbox by ID without checking that it
// exists.
func (c *Client) Sandbox(id string) *Sandbox {
	return &Sandbox{id: id, client: c}
}

// do performs an HTTP request against the API service. Non-2xx responses
// are converted to errors.
func (c *Client) do(ctx context.Context, method, path string, contentType string, body io.Reader) (*http.Response, error) {
	u := c.endpoint + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("sandbox: building request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox: reaching the API at %q: %w", c.endpoint, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var apiErr guest.Error
	if jsonErr := json.Unmarshal(payload, &apiErr); jsonErr == nil && apiErr.Message != "" {
		if apiErr.Code == guest.CodeNotFound {
			return nil, fmt.Errorf("sandbox: %w: %s", ErrNotFound, apiErr.Message)
		}
		return nil, fmt.Errorf("sandbox: %s", apiErr.Message)
	}
	return nil, fmt.Errorf("sandbox: API returned HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(payload))
}

// doJSON performs a request with an optional JSON body (in) and decodes
// the JSON response into out when non-nil.
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	contentType := ""
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("sandbox: encoding request: %w", err)
		}
		body = bytes.NewReader(data)
		contentType = "application/json"
	}
	resp, err := c.do(ctx, method, path, contentType, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("sandbox: decoding response: %w", err)
	}
	return nil
}
