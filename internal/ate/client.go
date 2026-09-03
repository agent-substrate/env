// Package ate implements the environment abstraction directly on top of
// Agent Substrate; it backs the ate-env-api service. A
// Environment wraps a Substrate actor: it can be created and deleted, and while running it
// accepts remote command execution and filesystem operations served by the
// ate-env-guest daemon inside the actor.
//
// Lifecycle operations go to the ateapi gRPC control plane; command and
// filesystem operations go through the atenet HTTP router, which routes
// requests by Host header to the actor's guest daemon.
//
// Clients outside this repository use the env package, which talks to
// the ate-env-api service instead.
package ate

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// DefaultHostSuffix is the DNS suffix the atenet router uses to identify
// actors: requests with Host "<id>.<suffix>" are routed to actor <id>.
const DefaultHostSuffix = "actors.resources.substrate.ate.dev"

// DefaultControlAddr and DefaultRouterAddr are the in-cluster addresses of
// the Substrate control plane and router, as installed by Agent Substrate
// in the ate-system namespace. They are the defaults for a ate-env-api running
// inside the cluster.
const (
	DefaultControlAddr = "api.ate-system.svc.cluster.local:443"
	DefaultRouterAddr  = "atenet-router.ate-system.svc.cluster.local:80"
)

// DefaultAtespace is the Substrate atespace every environment actor lives in.
const DefaultAtespace = "default"

// ErrNotFound is returned when an env, file, or directory does not exist.
var ErrNotFound = errors.New("not found")

// Options configures a Client.
type Options struct {
	// ControlAddr is the ateapi gRPC endpoint, e.g. "localhost:8080"
	// (typically a port-forward of svc/api in ate-system). Required.
	ControlAddr string

	// RouterAddr is the atenet HTTP router endpoint, e.g. "localhost:8000"
	// (typically a port-forward of svc/atenet-router in ate-system).
	// Required for Cmd and filesystem operations.
	RouterAddr string

	// HostSuffix overrides the router host suffix. Defaults to
	// DefaultHostSuffix.
	HostSuffix string

	// SkipVerify disables TLS certificate verification on the control
	// plane connection. Set this when talking to a port-forwarded ateapi,
	// which serves with pod certificates not signed by a public CA (the
	// Substrate demos and kubectl-ate connect the same way).
	SkipVerify bool

	// TLSConfig overrides the TLS configuration for the control plane
	// connection.
	TLSConfig *tls.Config

	// BearerTokenFile, if set, is a file whose contents are sent as a
	// bearer token on every control-plane RPC. Point it at a projected
	// ServiceAccount token with the ateapi audience when the control
	// plane requires authentication. The file is re-read on each RPC so
	// rotated tokens are picked up.
	BearerTokenFile string
}

// Client manages environments on a Substrate cluster.
type Client struct {
	opts    Options
	conn    *grpc.ClientConn
	control ateapipb.ControlClient

	mu        sync.Mutex
	guestPool map[string]*grpc.ClientConn
}

// New creates a Client. The control-plane connection is established
// lazily on first use.
func New(opts Options) (*Client, error) {
	if opts.ControlAddr == "" {
		return nil, errors.New("ate: Options.ControlAddr is required")
	}
	if opts.HostSuffix == "" {
		opts.HostSuffix = DefaultHostSuffix
	}

	var creds credentials.TransportCredentials
	if opts.TLSConfig != nil {
		creds = credentials.NewTLS(opts.TLSConfig)
	} else {
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: opts.SkipVerify})
	}

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if opts.BearerTokenFile != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(fileTokenCreds{path: opts.BearerTokenFile}))
	}
	conn, err := grpc.NewClient(opts.ControlAddr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("ate: dialing control plane: %w", err)
	}

	return &Client{
		opts:    opts,
		conn:    conn,
		control: ateapipb.NewControlClient(conn),
	}, nil
}

// Close releases the control-plane connection and any cached guest connections.
func (c *Client) Close() error {
	c.mu.Lock()
	for _, conn := range c.guestPool {
		_ = conn.Close()
	}
	c.guestPool = nil
	c.mu.Unlock()
	return c.conn.Close()
}

// DialGuest dials the guest daemon inside actor id in atespace via the atenet router.
func (c *Client) DialGuest(atespace, id string) (*grpc.ClientConn, error) {
	if c.opts.RouterAddr == "" {
		return nil, errors.New("ate: Options.RouterAddr is required for guest operations")
	}
	if id == "" {
		return nil, errors.New("ate: id is required")
	}
	if atespace == "" {
		atespace = DefaultAtespace
	}
	key := atespace + "/" + id

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guestPool == nil {
		c.guestPool = make(map[string]*grpc.ClientConn)
	}
	if conn, ok := c.guestPool[key]; ok {
		return conn, nil
	}

	authority := id + "." + atespace + "." + c.opts.HostSuffix
	conn, err := grpc.NewClient(
		c.opts.RouterAddr,
		grpc.WithAuthority(authority),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("ate: dialing guest %s: %w", key, err)
	}
	c.guestPool[key] = conn
	return conn, nil
}

// fileTokenCreds sends the contents of a file as a bearer token on each RPC.
type fileTokenCreds struct{ path string }

func (c fileTokenCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	b, err := os.ReadFile(c.path)
	if err != nil {
		return nil, fmt.Errorf("ate: reading bearer token file: %w", err)
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(b))}, nil
}

func (c fileTokenCreds) RequireTransportSecurity() bool { return true }

// EnsureAtespace creates the atespace with name if it does not already exist.
// If name is empty, it defaults to DefaultAtespace.
func (c *Client) EnsureAtespace(ctx context.Context, name string) error {
	if name == "" {
		name = DefaultAtespace
	}
	_, err := c.control.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{
			Metadata: &ateapipb.ResourceMetadata{
				Name: name,
			},
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return wrapGRPCError(err)
	}
	return nil
}

// CreateOptions configures the creation of a new actor.
type CreateOptions struct {
	ID       string
	Template string
	Atespace string
}

// Create registers a new actor from opts and starts it. ID and Template are
// required. Atespace is the Substrate atespace the actor and template live in.
func (c *Client) Create(ctx context.Context, opts CreateOptions) error {
	if opts.ID == "" {
		return errors.New("ate: CreateOptions.ID is required")
	}
	if opts.Template == "" {
		return errors.New("ate: CreateOptions.Template is required")
	}
	atespace := opts.Atespace
	if atespace == "" {
		atespace = DefaultAtespace
	}

	if err := c.EnsureAtespace(ctx, atespace); err != nil {
		return fmt.Errorf("ate: creating %q: %w", opts.ID, err)
	}

	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: atespace,
			Name:     opts.ID,
		},
		ActorTemplate: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     opts.Template,
		},
	}
	if _, err := c.control.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: actor}); err != nil {
		return fmt.Errorf("ate: creating %q: %w", opts.ID, wrapGRPCError(err))
	}

	return nil
}

// Get retrieves the actor with ID in atespace from the control plane.
// If atespace is empty, DefaultAtespace is used.
func (c *Client) Get(ctx context.Context, atespace, id string) (*ateapipb.Actor, error) {
	if id == "" {
		return nil, errors.New("ate: id is required")
	}
	actor, err := c.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: c.ref(atespace, id)})
	if err != nil {
		return nil, fmt.Errorf("ate: getting %q: %w", id, wrapGRPCError(err))
	}
	return actor, nil
}

// Suspend checkpoints and stops the actor with ID in atespace.
// If atespace is empty, DefaultAtespace is used.
func (c *Client) Suspend(ctx context.Context, atespace, id string) error {
	if _, err := c.control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: c.ref(atespace, id)}); err != nil {
		return fmt.Errorf("ate: suspending %q: %w", id, wrapGRPCError(err))
	}
	return nil
}

// Delete removes the actor with ID in atespace permanently. Substrate only deletes suspended
// actors, so Delete suspends the actor first.
// If atespace is empty, DefaultAtespace is used.
func (c *Client) Delete(ctx context.Context, atespace, id string) error {
	if err := c.Suspend(ctx, atespace, id); err != nil {
		return err
	}
	_, err := c.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: c.ref(atespace, id)})
	if err != nil {
		return fmt.Errorf("actor: deleting %q: %w", id, wrapGRPCError(err))
	}
	return nil
}

// ref returns the ObjectRef identifying the actor backing actor id in atespace.
// If atespace is empty, DefaultAtespace is used.
func (c *Client) ref(atespace, id string) *ateapipb.ObjectRef {
	if atespace == "" {
		atespace = DefaultAtespace
	}
	return &ateapipb.ObjectRef{Atespace: atespace, Name: id}
}

func wrapGRPCError(err error) error {
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, status.Convert(err).Message())
	}
	return err
}

// TODO(jbd): Allow setting a non-default atespace.
// TODO(jbd): Allow setting default atespace.
