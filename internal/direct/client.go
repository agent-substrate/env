// Package direct implements the sandbox abstraction directly on top of
// Agent Substrate; it backs the sbx-api service. A
// Sandbox wraps a Substrate actor: it can be created, suspended (full
// snapshot to object storage), resumed, and deleted, and while running it
// accepts remote command execution and filesystem operations served by the
// sbx-guest daemon inside the actor.
//
// Lifecycle operations go to the ateapi gRPC control plane; command and
// filesystem operations go through the atenet HTTP router, which routes
// requests by Host header to the actor's guest daemon.
//
// Clients outside this repository use the sandbox package, which talks to
// the sbx-api service instead.
package direct

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"

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
// in the ate-system namespace. They are the defaults for a sbx-api running
// inside the cluster.
const (
	DefaultControlAddr = "ateapi.ate-system.svc:443"
	DefaultRouterAddr  = "atenet-router.ate-system.svc:80"
)

// ErrNotFound is returned when a sandbox, file, or directory does not exist.
var ErrNotFound = errors.New("not found")

// Options configures a Client.
type Options struct {
	// ControlAddr is the ateapi gRPC endpoint, e.g. "localhost:8080"
	// (typically a port-forward of svc/ateapi in ate-system). Required.
	ControlAddr string

	// RouterAddr is the atenet HTTP router endpoint, e.g. "localhost:8000"
	// (typically a port-forward of svc/atenet-router in ate-system).
	// Required for Cmd and filesystem operations.
	RouterAddr string

	// HostSuffix overrides the router host suffix. Defaults to
	// DefaultHostSuffix.
	HostSuffix string

	// Template is the name of the default ActorTemplate for Create.
	Template string

	// Namespace is the Kubernetes namespace the ActorTemplates live in.
	// Defaults to "default".
	Namespace string

	// Atespace is the Substrate atespace sandboxes live in. Empty means
	// the global scope.
	Atespace string

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

	// AutoResume makes guest operations (Cmd, file I/O) resume a
	// suspended or paused sandbox and retry once, instead of failing.
	AutoResume bool
}

// Client manages sandboxes on a Substrate cluster.
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
		return nil, errors.New("sandbox: Options.ControlAddr is required")
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
		return nil, fmt.Errorf("sandbox: dialing control plane: %w", err)
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

func (c *Client) templateRef(overrideNamespace, overrideName string) (namespace, name string, err error) {
	name = overrideName
	if name == "" {
		name = c.opts.Template
	}
	if name == "" {
		return "", "", errors.New("sandbox: no ActorTemplate specified (set Options.Template or WithTemplate)")
	}
	namespace = overrideNamespace
	if namespace == "" {
		namespace = c.opts.Namespace
	}
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
	workerSelector    map[string]string
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

// WithWorkerSelector constrains which worker pools can host the sandbox.
func WithWorkerSelector(matchLabels map[string]string) CreateOption {
	return func(c *createConfig) { c.workerSelector = matchLabels }
}

// Create registers a new sandbox with the given ID (a DNS-1123 label) and
// starts it.
func (c *Client) Create(ctx context.Context, id string, opts ...CreateOption) (*Sandbox, error) {
	var cfg createConfig
	for _, o := range opts {
		o(&cfg)
	}
	namespace, name, err := c.templateRef(cfg.templateNamespace, cfg.template)
	if err != nil {
		return nil, err
	}

	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: c.opts.Atespace,
			Name:     id,
		},
		ActorTemplateNamespace: namespace,
		ActorTemplateName:      name,
	}
	if len(cfg.workerSelector) > 0 {
		actor.WorkerSelector = &ateapipb.Selector{MatchLabels: cfg.workerSelector}
	}
	if _, err := c.control.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: actor}); err != nil {
		return nil, fmt.Errorf("sandbox: creating %q: %w", id, wrapGRPCError(err))
	}

	sb := &Sandbox{id: id, client: c}
	if _, err := c.control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: c.ref(id)}); err != nil {
		return sb, fmt.Errorf("sandbox: starting %q: %w", id, wrapGRPCError(err))
	}
	return sb, nil
}

// ref returns the ObjectRef identifying the actor backing sandbox id.
func (c *Client) ref(id string) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: c.opts.Atespace, Name: id}
}

// Open returns a handle to an existing sandbox, verifying it exists.
func (c *Client) Open(ctx context.Context, id string) (*Sandbox, error) {
	sb := &Sandbox{id: id, client: c}
	if _, err := sb.Info(ctx); err != nil {
		return nil, err
	}
	return sb, nil
}

// Sandbox returns a handle to a sandbox by ID without checking that it
// exists.
func (c *Client) Sandbox(id string) *Sandbox {
	return &Sandbox{id: id, client: c}
}

func wrapGRPCError(err error) error {
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, status.Convert(err).Message())
	}
	return err
}
