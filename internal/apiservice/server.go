// Package apiservice implements the EnvironmentService, ProcessService, and
// FileSystemService gRPC APIs for the ate-env API server, proxying in-actor
// execution and filesystem operations directly to the guest daemon via atenet.
package apiservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/agent-substrate/env/internal/ate"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// DefaultTemplate is the ActorTemplate name used when a create request
// does not specify one.
const DefaultTemplate = "default-env"

// DefaultNamespace is the default Kubernetes namespace used when deploying
// ate-env manifests.
const DefaultNamespace = "ate-env"

// DefaultAtespace is the Substrate atespace used when a request does not
// specify one.
const DefaultAtespace = "default"

// Server implements ateenvv1alpha.EnvironmentServiceServer, ateenvv1alpha.ProcessServiceServer,
// and ateenvv1alpha.FileSystemServiceServer.
type Server struct {
	ateenvv1alpha.UnimplementedEnvironmentServiceServer
	ateenvv1alpha.UnimplementedProcessServiceServer
	ateenvv1alpha.UnimplementedFileSystemServiceServer

	client     *ate.Client
	routerAddr string
	hostSuffix string
}

// New creates a new Server.
func New(client *ate.Client, routerAddr, hostSuffix string) *Server {
	if hostSuffix == "" {
		hostSuffix = ate.DefaultHostSuffix
	}
	routerAddr = strings.TrimPrefix(routerAddr, "http://")
	routerAddr = strings.TrimPrefix(routerAddr, "https://")
	return &Server{
		client:     client,
		routerAddr: routerAddr,
		hostSuffix: hostSuffix,
	}
}

// Close is a no-op retained for lifecycle compatibility.
func (s *Server) Close() {}

// ============================================================================
// --- ENVIRONMENT SERVICE ---
// ============================================================================

// CreateEnvironment registers and starts a new environment.
func (s *Server) CreateEnvironment(ctx context.Context, req *ateenvv1alpha.CreateEnvironmentRequest) (*ateenvv1alpha.CreateEnvironmentResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	tmpl := req.GetTemplate()
	templateName := tmpl.GetName()
	if templateName == "" {
		templateName = DefaultTemplate
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = tmpl.GetAtespace()
	}
	if atespace == "" {
		atespace = DefaultAtespace
	}

	opts := ate.CreateOptions{
		ID:       req.GetId(),
		Template: templateName,
		Atespace: atespace,
	}
	if err := s.client.Create(ctx, opts); err != nil {
		return nil, toGRPCError(err)
	}

	return &ateenvv1alpha.CreateEnvironmentResponse{
		Environment: &ateenvv1alpha.Environment{
			Id:       req.GetId(),
			Atespace: atespace,
			Template: &ateenvv1alpha.Template{
				Name:     templateName,
				Atespace: atespace,
			},
			Status: ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED,
		},
	}, nil
}

// GetEnvironment retrieves the status and configuration of an existing environment.
func (s *Server) GetEnvironment(ctx context.Context, req *ateenvv1alpha.GetEnvironmentRequest) (*ateenvv1alpha.GetEnvironmentResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}

	actor, err := s.client.Get(ctx, atespace, req.GetId())
	if err != nil {
		return nil, toGRPCError(err)
	}

	return &ateenvv1alpha.GetEnvironmentResponse{
		Environment: ActorToEnvironment(actor),
	}, nil
}

// SuspendEnvironment checkpoints and stops an active environment.
func (s *Server) SuspendEnvironment(ctx context.Context, req *ateenvv1alpha.SuspendEnvironmentRequest) (*ateenvv1alpha.SuspendEnvironmentResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}

	if err := s.client.Suspend(ctx, atespace, req.GetId()); err != nil {
		return nil, toGRPCError(err)
	}

	return &ateenvv1alpha.SuspendEnvironmentResponse{}, nil
}

// DeleteEnvironment permanently removes an environment and its resources.
func (s *Server) DeleteEnvironment(ctx context.Context, req *ateenvv1alpha.DeleteEnvironmentRequest) (*ateenvv1alpha.DeleteEnvironmentResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}

	if err := s.client.Delete(ctx, atespace, req.GetId()); err != nil {
		return nil, toGRPCError(err)
	}

	return &ateenvv1alpha.DeleteEnvironmentResponse{}, nil
}

// ============================================================================
// --- PROCESS SERVICE ---
// ============================================================================

// StartProcess launches a process inside the target environment container.
func (s *Server) StartProcess(ctx context.Context, req *ateenvv1alpha.StartProcessRequest) (*ateenvv1alpha.StartProcessResponse, error) {
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := s.guestConn(atespace, envID)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	outCtx := forwardOutgoingContext(ctx)
	return ateenvv1alpha.NewProcessServiceClient(conn).StartProcess(outCtx, req)
}

// GetProcess retrieves the status of a process running inside the environment.
func (s *Server) GetProcess(ctx context.Context, req *ateenvv1alpha.GetProcessRequest) (*ateenvv1alpha.Process, error) {
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := s.guestConn(atespace, envID)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	outCtx := forwardOutgoingContext(ctx)
	return ateenvv1alpha.NewProcessServiceClient(conn).GetProcess(outCtx, req)
}

// StreamProcessOutputs streams real-time stdout and stderr from a process.
func (s *Server) StreamProcessOutputs(req *ateenvv1alpha.StreamProcessOutputsRequest, stream grpc.ServerStreamingServer[ateenvv1alpha.OutputChunk]) error {
	ctx := stream.Context()
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return err
	}
	conn, err := s.guestConn(atespace, envID)
	if err != nil {
		return err
	}
	defer conn.Close()

	outCtx := forwardOutgoingContext(ctx)
	clientStream, err := ateenvv1alpha.NewProcessServiceClient(conn).StreamProcessOutputs(outCtx, req)
	if err != nil {
		return err
	}

	for {
		chunk, err := clientStream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(chunk); err != nil {
			return err
		}
	}
}

// KillProcess terminates a running process inside the environment.
func (s *Server) KillProcess(ctx context.Context, req *ateenvv1alpha.KillProcessRequest) (*ateenvv1alpha.KillProcessResponse, error) {
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := s.guestConn(atespace, envID)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	outCtx := forwardOutgoingContext(ctx)
	return ateenvv1alpha.NewProcessServiceClient(conn).KillProcess(outCtx, req)
}

// ============================================================================
// --- FILESYSTEM SERVICE ---
// ============================================================================

// ReadFile streams file contents from the target environment.
func (s *Server) ReadFile(req *ateenvv1alpha.ReadFileRequest, stream grpc.ServerStreamingServer[ateenvv1alpha.FileChunk]) error {
	ctx := stream.Context()
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return err
	}
	conn, err := s.guestConn(atespace, envID)
	if err != nil {
		return err
	}
	defer conn.Close()

	outCtx := forwardOutgoingContext(ctx)
	clientStream, err := ateenvv1alpha.NewFileSystemServiceClient(conn).ReadFile(outCtx, req)
	if err != nil {
		return err
	}

	for {
		chunk, err := clientStream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(chunk); err != nil {
			return err
		}
	}
}

// WriteFile streams file contents to the target environment.
func (s *Server) WriteFile(stream grpc.ClientStreamingServer[ateenvv1alpha.WriteFileRequest, ateenvv1alpha.WriteFileResponse]) error {
	ctx := stream.Context()
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return err
	}
	conn, err := s.guestConn(atespace, envID)
	if err != nil {
		return err
	}
	defer conn.Close()

	outCtx := forwardOutgoingContext(ctx)
	clientStream, err := ateenvv1alpha.NewFileSystemServiceClient(conn).WriteFile(outCtx)
	if err != nil {
		return err
	}

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := clientStream.Send(req); err != nil {
			return err
		}
	}

	resp, err := clientStream.CloseAndRecv()
	if err != nil {
		return err
	}
	return stream.SendAndClose(resp)
}

// ============================================================================
// --- HELPERS ---
// ============================================================================

func envFromContext(ctx context.Context) (string, string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", "", status.Error(codes.InvalidArgument, "missing environment metadata in request context")
	}
	ids := md.Get("x-env-id")
	if len(ids) == 0 || ids[0] == "" {
		return "", "", status.Error(codes.InvalidArgument, "x-env-id header is required")
	}
	atespaces := md.Get("x-env-atespace")
	atespace := DefaultAtespace
	if len(atespaces) > 0 && atespaces[0] != "" {
		atespace = atespaces[0]
	}
	return ids[0], atespace, nil
}

func forwardOutgoingContext(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, md.Copy())
}

func (s *Server) guestConn(atespace, id string) (*grpc.ClientConn, error) {
	if s.routerAddr == "" {
		return nil, status.Error(codes.FailedPrecondition, "router address not configured for guest operations")
	}
	if atespace == "" {
		atespace = DefaultAtespace
	}
	authority := fmt.Sprintf("%s.%s.%s", id, atespace, s.hostSuffix)
	conn, err := grpc.NewClient(
		s.routerAddr,
		grpc.WithAuthority(authority),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "dialing guest router for %s/%s: %v", atespace, id, err)
	}
	return conn, nil
}

// ActorToEnvironment converts an ateapipb Actor to an ateenvv1alpha Environment.
func ActorToEnvironment(actor *ateapipb.Actor) *ateenvv1alpha.Environment {
	if actor == nil {
		return nil
	}
	templateName := ""
	templateAtespace := ""
	if tmpl := actor.GetActorTemplate(); tmpl != nil {
		templateName = tmpl.GetName()
		templateAtespace = tmpl.GetAtespace()
	}
	return &ateenvv1alpha.Environment{
		Id:       actor.GetMetadata().GetName(),
		Atespace: actor.GetMetadata().GetAtespace(),
		Template: &ateenvv1alpha.Template{
			Name:     templateName,
			Atespace: templateAtespace,
		},
		Status: ActorStatusToEnvStatus(actor.GetStatus()),
	}
}

// ActorStatusToEnvStatus maps an ateapipb ActorStatus to an ateenvv1alpha EnvironmentStatus.
func ActorStatusToEnvStatus(st *ateapipb.ActorStatus) ateenvv1alpha.EnvironmentStatus {
	if st == nil {
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED
	}
	switch st.GetState() {
	case ateapipb.ActorState_ACTOR_STATE_RESUMING:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RESUMING
	case ateapipb.ActorState_ACTOR_STATE_RUNNING:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDING
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED
	case ateapipb.ActorState_ACTOR_STATE_PAUSING:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_PAUSING
	case ateapipb.ActorState_ACTOR_STATE_PAUSED:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_PAUSED
	case ateapipb.ActorState_ACTOR_STATE_CRASHED:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_CRASHED
	case ateapipb.ActorState_ACTOR_STATE_DELETING:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_DELETING
	default:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED
	}
}

func toGRPCError(err error) error {
	if err == nil {
		return nil
	}
	if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, ate.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}
