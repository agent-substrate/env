// Package apiservice implements the EnvironmentService gRPC control plane API.
package apiservice

import (
	"context"
	"errors"

	"github.com/agent-substrate/env/internal/ate"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DefaultTemplate is the ActorTemplate name used when a create request
// does not specify one.
const DefaultTemplate = "default-env"

// DefaultNamespace is the Kubernetes namespace the ActorTemplate is
// looked up in when a create request does not specify one.
const DefaultNamespace = "ate-env"

// DefaultAtespace is the Substrate atespace used when a request does not
// specify one.
const DefaultAtespace = "default"

// Server implements ateenvv1.EnvironmentServiceServer backed by an ate.Client.
type Server struct {
	ateenvv1.UnimplementedEnvironmentServiceServer

	client *ate.Client
}

// New creates a new Server.
func New(client *ate.Client) *Server {
	return &Server{client: client}
}

// CreateEnvironment registers and starts a new environment.
func (s *Server) CreateEnvironment(ctx context.Context, req *ateenvv1.CreateEnvironmentRequest) (*ateenvv1.CreateEnvironmentResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	tmpl := req.GetTemplate()
	templateName := tmpl.GetName()
	if templateName == "" {
		templateName = DefaultTemplate
	}
	templateNamespace := tmpl.GetNamespace()
	if templateNamespace == "" {
		templateNamespace = DefaultNamespace
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}

	opts := ate.CreateOptions{
		ID:        req.GetId(),
		Template:  templateName,
		Namespace: templateNamespace,
		Atespace:  atespace,
	}
	if err := s.client.Create(ctx, opts); err != nil {
		return nil, toGRPCError(err)
	}

	actor, err := s.client.Get(ctx, atespace, req.GetId())
	if err != nil {
		return &ateenvv1.CreateEnvironmentResponse{
			Environment: &ateenvv1.Environment{
				Id:       req.GetId(),
				Atespace: atespace,
				Template: &ateenvv1.Template{
					Name:      templateName,
					Namespace: templateNamespace,
				},
				Status: ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING,
			},
		}, nil
	}

	return &ateenvv1.CreateEnvironmentResponse{
		Environment: ActorToEnvironment(actor),
	}, nil
}

// GetEnvironment retrieves the status and configuration of an existing environment.
func (s *Server) GetEnvironment(ctx context.Context, req *ateenvv1.GetEnvironmentRequest) (*ateenvv1.GetEnvironmentResponse, error) {
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

	return &ateenvv1.GetEnvironmentResponse{
		Environment: ActorToEnvironment(actor),
	}, nil
}

// SuspendEnvironment checkpoints and stops an active environment.
func (s *Server) SuspendEnvironment(ctx context.Context, req *ateenvv1.SuspendEnvironmentRequest) (*ateenvv1.SuspendEnvironmentResponse, error) {
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

	return &ateenvv1.SuspendEnvironmentResponse{}, nil
}

// DeleteEnvironment permanently removes an environment and its resources.
func (s *Server) DeleteEnvironment(ctx context.Context, req *ateenvv1.DeleteEnvironmentRequest) (*ateenvv1.DeleteEnvironmentResponse, error) {
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

	return &ateenvv1.DeleteEnvironmentResponse{}, nil
}

// ActorToEnvironment converts an ateapipb Actor to an ateenvv1 Environment.
func ActorToEnvironment(actor *ateapipb.Actor) *ateenvv1.Environment {
	if actor == nil {
		return nil
	}
	return &ateenvv1.Environment{
		Id:       actor.GetMetadata().GetName(),
		Atespace: actor.GetMetadata().GetAtespace(),
		Template: &ateenvv1.Template{
			Name:      actor.GetActorTemplateName(),
			Namespace: actor.GetActorTemplateNamespace(),
		},
		Status: ActorStatusToEnvStatus(actor.GetStatus()),
	}
}

// ActorStatusToEnvStatus maps an ateapipb Actor_Status to an ateenvv1 EnvironmentStatus.
func ActorStatusToEnvStatus(st ateapipb.Actor_Status) ateenvv1.EnvironmentStatus {
	switch st {
	case ateapipb.Actor_STATUS_RESUMING:
		return ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_PENDING
	case ateapipb.Actor_STATUS_RUNNING:
		return ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING
	case ateapipb.Actor_STATUS_SUSPENDING, ateapipb.Actor_STATUS_SUSPENDED:
		return ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED
	case ateapipb.Actor_STATUS_PAUSING, ateapipb.Actor_STATUS_PAUSED:
		return ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_PAUSED
	case ateapipb.Actor_STATUS_DELETING:
		return ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_TERMINATED
	default:
		return ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED
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
	case errors.Is(err, ate.ErrPrecondition):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}
