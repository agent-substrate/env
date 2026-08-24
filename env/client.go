// Package env is the Go SDK for the environment service. It talks to the
// ate-env-api service, which bridges to the Substrate
// control plane and router; `ate-env manifest` runs that service
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

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// ErrNotFound is returned when a env, file, or directory does not exist.
var ErrNotFound = errors.New("not found")

// ClientOptions configures a Client.
type ClientOptions struct {
	// Endpoint is the address or base URL of the ate-env-api service, e.g.
	// "http://localhost:7777" or "localhost:7777". Required if GRPCConn is nil.
	Endpoint string

	// GRPCConn overrides the grpc.ClientConn used for EnvironmentService RPCs.
	GRPCConn *grpc.ClientConn
}

// Client manages environments against a ate-env-api endpoint over gRPC (for lifecycle)
// and HTTP (for in-env guest proxying).
type Client struct {
	endpoint string
	opts     ClientOptions
	http     *http.Client
	grpcConn *grpc.ClientConn
	grpc     ateenvv1.EnvironmentServiceClient
}

// NewClient returns a Client targeting endpoint.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Endpoint == "" && opts.GRPCConn == nil {
		return nil, errors.New("env: ClientOptions.Endpoint is required")
	}
	endpoint := strings.TrimRight(opts.Endpoint, "/")
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") && endpoint != "" {
		endpoint = "http://" + endpoint
	}

	var (
		grpcConn *grpc.ClientConn
		err      error
	)
	if opts.GRPCConn != nil {
		grpcConn = opts.GRPCConn
	} else {
		target := strings.TrimPrefix(endpoint, "http://")
		target = strings.TrimPrefix(target, "https://")
		grpcConn, err = grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, fmt.Errorf("env: connecting to gRPC %s: %w", target, err)
		}
	}

	return &Client{
		endpoint: endpoint,
		opts:     opts,
		http:     http.DefaultClient,
		grpcConn: grpcConn,
		grpc:     ateenvv1.NewEnvironmentServiceClient(grpcConn),
	}, nil
}

// Close releases the client's resources.
func (c *Client) Close() error {
	if c.grpcConn != nil && c.opts.GRPCConn == nil {
		return c.grpcConn.Close()
	}
	return nil
}

// Create registers a new env with the parameters given in req and
// starts it using the gRPC EnvironmentService.
func (c *Client) Create(ctx context.Context, req *ateenvv1.CreateEnvironmentRequest) (*Env, error) {
	resp, err := c.grpc.CreateEnvironment(ctx, req)
	if err != nil {
		return nil, fromGRPCError(err)
	}
	created := resp.GetEnvironment()
	return &Env{
		id:       created.GetId(),
		atespace: created.GetAtespace(),
		client:   c,
	}, nil
}

// Suspend checkpoints and stops the environment using the gRPC EnvironmentService.
func (c *Client) Suspend(ctx context.Context, atespace, id string) error {
	req := &ateenvv1.SuspendEnvironmentRequest{
		Id:       id,
		Atespace: atespace,
	}
	_, err := c.grpc.SuspendEnvironment(ctx, req)
	if err != nil {
		return fromGRPCError(err)
	}
	return nil
}

// Delete removes the environment permanently using the gRPC EnvironmentService.
func (c *Client) Delete(ctx context.Context, atespace, id string) error {
	req := &ateenvv1.DeleteEnvironmentRequest{
		Id:       id,
		Atespace: atespace,
	}
	_, err := c.grpc.DeleteEnvironment(ctx, req)
	if err != nil {
		return fromGRPCError(err)
	}
	return nil
}

// List returns the environments in atespace. An empty atespace lists
// environments across all atespaces.
func (c *Client) List(ctx context.Context, atespace string) ([]EnvInfo, error) {
	var infos []EnvInfo
	path := "/v1/envs"
	if atespace != "" {
		path += "?" + url.Values{"atespace": {atespace}}.Encode()
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &infos); err != nil {
		return nil, err
	}
	return infos, nil
}

// Get returns the environment with the given id in atespace.
func (c *Client) Get(ctx context.Context, atespace, id string) (*EnvInfo, error) {
	path := "/v1/envs/" + url.PathEscape(id)
	if atespace != "" {
		path += "?" + url.Values{"atespace": {atespace}}.Encode()
	}
	var info EnvInfo
	if err := c.do(ctx, http.MethodGet, path, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Env returns a handle to an environment without checking that it exists.
func (c *Client) Env(atespace, id string) *Env {
	return &Env{
		id:       id,
		atespace: atespace,
		client:   c,
	}
}

func fromGRPCError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("env: %w: %s", ErrNotFound, status.Convert(err).Message())
	}
	return fmt.Errorf("env: %w", err)
}

// do performs an HTTP request against the API service with an optional JSON
// body (in), and decodes the JSON response into out when non-nil. Non-2xx
// responses are converted to errors.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if c.endpoint == "" {
		return errors.New("env: ClientOptions.Endpoint is required for this operation; a GRPCConn only covers the lifecycle RPCs")
	}
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
