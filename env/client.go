// Package env is the Go SDK for the environment service. It talks to the
// ate-env-api service, which bridges to the Substrate
// control plane and router; `ate-env deploy` runs that service
// in-cluster.
package env

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrNotFound is returned when a env, file, or directory does not exist.
var ErrNotFound = errors.New("not found")

// ClientOptions configures a Client.
type ClientOptions struct {
	// Endpoint is the base URL of the ate-env-api service, e.g.
	// "http://localhost:7777". Required.
	Endpoint string

	// HTTPClient overrides the http.Client used for API requests.
	HTTPClient *http.Client
}

// Client manages environments against a ate-env-api endpoint over HTTP.
type Client struct {
	endpoint string
	opts     ClientOptions
	http     *http.Client
}

// NewClient returns a Client targeting endpoint.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("env: ClientOptions.Endpoint is required")
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

// Create registers a new env with the ID given in req (a DNS-1123 label) and
// starts it. An empty Template or Namespace falls back to the service's
// default.
func (c *Client) Create(ctx context.Context, req CreateRequest) (*Env, error) {
	if err := c.do(ctx, http.MethodPost, "/v1/envs", req, nil); err != nil {
		return nil, err
	}
	return &Env{id: req.ID, client: c}, nil
}

// Fork creates the environment destID from the latest snapshot of the
// environment id, inheriting its template, and returns a handle to it.
// It fails if the source environment is not suspended.
func (c *Client) Fork(ctx context.Context, id, destID string) (*Env, error) {
	req := ForkRequest{DestID: destID}
	path := "/v1/envs/" + url.PathEscape(id) + "/fork"
	if err := c.do(ctx, http.MethodPost, path, req, nil); err != nil {
		return nil, err
	}
	return &Env{id: destID, client: c}, nil
}

// Env returns a handle to an environment by ID without checking that it
// exists.
func (c *Client) Env(id string) *Env {
	return &Env{id: id, client: c}
}

// do performs an HTTP request against the API service with an optional JSON
// body (in), and decodes the JSON response into out when non-nil. Non-2xx
// responses are converted to errors.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("env: encoding request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return fmt.Errorf("env: building request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("env: reaching the API at %q: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var apiErr Error
		if jsonErr := json.Unmarshal(payload, &apiErr); jsonErr == nil && apiErr.Message != "" {
			if apiErr.Code == CodeNotFound {
				return fmt.Errorf("env: %w: %s", ErrNotFound, apiErr.Message)
			}
			return fmt.Errorf("env: %s", apiErr.Message)
		}
		return fmt.Errorf("env: API returned HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(payload))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("env: decoding response: %w", err)
	}
	return nil
}
