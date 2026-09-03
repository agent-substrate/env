// Package env is the Go SDK for the environment service. It talks to the
// ate-env-api service, which bridges to the Substrate
// control plane and router; `ate-env manifest` runs that service
// in-cluster.
package env

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/agent-substrate/env/internal/ate"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// ErrNotFound is returned when an env, file, or directory does not exist.
var ErrNotFound = errors.New("not found")

// ClientOptions configures a Client.
type ClientOptions struct {
	// Endpoint is the address or base URL of the ate-env-api service, e.g.
	// "http://localhost:7777" or "localhost:7777". Required if GRPCConn is nil.
	Endpoint string

	// GRPCConn overrides the grpc.ClientConn used for ate-env-api RPCs.
	GRPCConn *grpc.ClientConn
}

// Client manages environments against an ate-env-api endpoint over gRPC.
type Client struct {
	endpoint   string
	opts       ClientOptions
	grpcConn   *grpc.ClientConn
	grpc       ateenvv1.EnvironmentServiceClient
	process    ateenvv1.ProcessServiceClient
	filesystem ateenvv1.FileSystemServiceClient
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
		endpoint:   endpoint,
		opts:       opts,
		grpcConn:   grpcConn,
		grpc:       ateenvv1.NewEnvironmentServiceClient(grpcConn),
		process:    ateenvv1.NewProcessServiceClient(grpcConn),
		filesystem: ateenvv1.NewFileSystemServiceClient(grpcConn),
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
	return c.Env(created.GetAtespace(), created.GetId()), nil
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

// Env returns a handle to an environment without checking that it exists.
func (c *Client) Env(atespace, id string) *Env {
	if atespace == "" {
		atespace = ate.DefaultAtespace
	}
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
