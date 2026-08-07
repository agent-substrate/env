// Package ate implements the environment abstraction directly on top of
// Agent Substrate; it backs the ate-env-api service. A
// Environment wraps a Substrate actor: it can be created, suspended (full
// snapshot to object storage), resumed, and deleted, and while running it
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
	"net/http"
	"net/http/httputil"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
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

	// HTTPClient overrides the HTTP client used for router traffic.
	HTTPClient *http.Client
}

// Client manages environments on a Substrate cluster.
type Client struct {
	opts    Options
	conn    *grpc.ClientConn
	control ateapipb.ControlClient
	http    *http.Client
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

	conn, err := grpc.NewClient(opts.ControlAddr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("ate: dialing control plane: %w", err)
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Client{
		opts:    opts,
		conn:    conn,
		control: ateapipb.NewControlClient(conn),
		http:    httpClient,
	}, nil
}

// Close releases the control-plane connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// ProxyGuest reverse-proxies an HTTP request to the guest daemon inside env id.
// subPath is the path on guest, e.g. "/v1/cmd" or "/v1/file".
func (c *Client) ProxyGuest(id string, subPath string, w http.ResponseWriter, r *http.Request) {
	if c.opts.RouterAddr == "" {
		http.Error(w, `{"code":"internal","error":"ate: Options.RouterAddr is required for command and filesystem operations"}`, http.StatusInternalServerError)
		return
	}
	director := func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = c.opts.RouterAddr
		req.URL.Path = subPath
		req.Host = id + ".default." + c.opts.HostSuffix
	}
	transport := c.http.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	proxy := &httputil.ReverseProxy{
		Director:  director,
		Transport: transport,
	}
	proxy.ServeHTTP(w, r)
}

func (c *Client) templateRef(overrideNamespace, overrideName string) (namespace, name string, err error) {
	name = overrideName
	if name == "" {
		return "", "", errors.New("ate: no ActorTemplate specified (use WithTemplate)")
	}
	namespace = overrideNamespace
	if namespace == "" {
		namespace = "default"
	}
	return namespace, name, nil
}

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

// EnsureAtespace creates the atespace with name if it does not already exist.
// If name is empty, it defaults to "default".
func (c *Client) EnsureAtespace(ctx context.Context, name string) error {
	if name == "" {
		name = "default"
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

// Create registers a new actor with the given ID (a DNS-1123 label) and
// starts it.
func (c *Client) Create(ctx context.Context, id string, opts ...CreateOption) (*ActorClient, error) {
	var cfg createConfig
	for _, o := range opts {
		o(&cfg)
	}
	namespace, name, err := c.templateRef(cfg.templateNamespace, cfg.template)
	if err != nil {
		return nil, err
	}

	const atespace = "default"
	if err := c.EnsureAtespace(ctx, atespace); err != nil {
		return nil, fmt.Errorf("ate: creating %q: %w", id, err)
	}

	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: atespace,
			Name:     id,
		},
		ActorTemplateNamespace: namespace,
		ActorTemplateName:      name,
	}
	if _, err := c.control.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: actor}); err != nil {
		return nil, fmt.Errorf("ate: creating %q: %w", id, wrapGRPCError(err))
	}

	sb := &ActorClient{id: id, client: c}
	return sb, nil
}

// ref returns the ObjectRef identifying the actor backing actor id.
func (c *Client) ref(id string) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: "default", Name: id}
}

// Actor returns a handle to an actor by ID without checking that it
// exists.
func (c *Client) Actor(id string) *ActorClient {
	return &ActorClient{id: id, client: c}
}

func wrapGRPCError(err error) error {
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, status.Convert(err).Message())
	}
	return err
}

// TODO(jbd): Allow setting a non-default atespace.
// TODO(jbd): Remove createConfig, apply changes to ateapipb.Actor.
// TODO(jbd): Allow setting default atespace.
