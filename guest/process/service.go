package process

import (
	"context"
	"errors"
	"os"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements ateenvv1alpha.ProcessServiceServer.
// It provides in-actor asynchronous process execution and log streaming for cmd/ate-env-guest.
type Service struct {
	ateenvv1alpha.UnimplementedProcessServiceServer
	tracker *Tracker
}

// NewService creates a new ProcessServiceServer instance.
func NewService(tracker *Tracker) *Service {
	return &Service{
		tracker: tracker,
	}
}

// StartProcess launches a process asynchronously in the background.
func (s *Service) StartProcess(ctx context.Context, req *ateenvv1alpha.StartProcessRequest) (*ateenvv1alpha.StartProcessResponse, error) {
	if len(req.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command cannot be empty")
	}

	state, err := s.tracker.Start(req.GetCommand(), req.GetCwd(), req.GetEnv())
	if err != nil {
		return nil, err
	}

	return &ateenvv1alpha.StartProcessResponse{
		ProcessId: state.ProcessID,
	}, nil
}

// GetProcess returns the metadata, status, and exit code of a process.
func (s *Service) GetProcess(ctx context.Context, req *ateenvv1alpha.GetProcessRequest) (*ateenvv1alpha.Process, error) {
	if req.GetProcessId() == "" {
		return nil, status.Error(codes.InvalidArgument, "process_id cannot be empty")
	}

	state, ok := s.tracker.Get(req.GetProcessId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %q not found", req.GetProcessId())
	}

	return state.ToProto(), nil
}

// StreamProcessOutputs streams stdout and stderr in real-time or as a snapshot.
func (s *Service) StreamProcessOutputs(req *ateenvv1alpha.StreamProcessOutputsRequest, stream ateenvv1alpha.ProcessService_StreamProcessOutputsServer) error {
	if req.GetProcessId() == "" {
		return status.Error(codes.InvalidArgument, "process_id cannot be empty")
	}

	state, ok := s.tracker.Get(req.GetProcessId())
	if !ok {
		return status.Errorf(codes.NotFound, "process %q not found", req.GetProcessId())
	}

	stdoutOffset := req.GetStdoutOffset()
	stderrOffset := req.GetStderrOffset()
	follow := req.GetFollow()
	ctx := stream.Context()

	for {
		// Read stdout delta
		stdoutBytes, newStdoutOffset, err := ReadLogs(state.StdoutPath, stdoutOffset)
		if err != nil {
			return status.Errorf(codes.Internal, "reading stdout: %v", err)
		}
		if len(stdoutBytes) > 0 {
			if err := stream.Send(&ateenvv1alpha.OutputChunk{
				Source: ateenvv1alpha.OutputSource_OUTPUT_SOURCE_STDOUT,
				Data:   stdoutBytes,
			}); err != nil {
				return err
			}
			stdoutOffset = newStdoutOffset
		}

		// Read stderr delta
		stderrBytes, newStderrOffset, err := ReadLogs(state.StderrPath, stderrOffset)
		if err != nil {
			return status.Errorf(codes.Internal, "reading stderr: %v", err)
		}
		if len(stderrBytes) > 0 {
			if err := stream.Send(&ateenvv1alpha.OutputChunk{
				Source: ateenvv1alpha.OutputSource_OUTPUT_SOURCE_STDERR,
				Data:   stderrBytes,
			}); err != nil {
				return err
			}
			stderrOffset = newStderrOffset
		}

		if !follow {
			// Snapshot mode: finish after reading available output up to this point
			return nil
		}

		// Check if process has finished and we consumed all output
		state.mu.RLock()
		isTerminated := state.Status != ateenvv1alpha.ProcessStatus_PROCESS_STATUS_RUNNING
		state.mu.RUnlock()

		if isTerminated {
			// Final check to see if there were any remaining bytes flushed on exit
			finalStdout, _, _ := ReadLogs(state.StdoutPath, stdoutOffset)
			if len(finalStdout) > 0 {
				_ = stream.Send(&ateenvv1alpha.OutputChunk{
					Source: ateenvv1alpha.OutputSource_OUTPUT_SOURCE_STDOUT,
					Data:   finalStdout,
				})
			}
			finalStderr, _, _ := ReadLogs(state.StderrPath, stderrOffset)
			if len(finalStderr) > 0 {
				_ = stream.Send(&ateenvv1alpha.OutputChunk{
					Source: ateenvv1alpha.OutputSource_OUTPUT_SOURCE_STDERR,
					Data:   finalStderr,
				})
			}
			return nil
		}

		// Sleep or wait for context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// KillProcess terminates a running process and returns its exit code.
func (s *Service) KillProcess(ctx context.Context, req *ateenvv1alpha.KillProcessRequest) (*ateenvv1alpha.KillProcessResponse, error) {
	if req.GetProcessId() == "" {
		return nil, status.Error(codes.InvalidArgument, "process_id cannot be empty")
	}

	exitCode, err := s.tracker.Kill(req.GetProcessId())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.NotFound, "process %q not found", req.GetProcessId())
		}
		return nil, status.Errorf(codes.Internal, "killing process: %v", err)
	}

	return &ateenvv1alpha.KillProcessResponse{
		ExitCode: exitCode,
	}, nil
}
