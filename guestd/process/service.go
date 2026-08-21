package process

import (
	"context"
	"errors"
	"os"
	"time"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements ateenvv1.ProcessServiceServer.
type Service struct {
	ateenvv1.UnimplementedProcessServiceServer
	tracker *Tracker
}

// NewService creates a new ProcessServiceServer instance.
func NewService(tracker *Tracker) *Service {
	return &Service{
		tracker: tracker,
	}
}

// StartProcess launches a process asynchronously in the background.
func (s *Service) StartProcess(ctx context.Context, req *ateenvv1.StartProcessRequest) (*ateenvv1.StartProcessResponse, error) {
	if len(req.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command cannot be empty")
	}

	state, err := s.tracker.Start(req.GetCommand(), req.GetCwd(), req.GetEnv())
	if err != nil {
		return nil, err
	}

	return &ateenvv1.StartProcessResponse{
		ProcessId: state.ProcessID,
	}, nil
}

// GetProcess returns the metadata, status, and exit code of a process.
func (s *Service) GetProcess(ctx context.Context, req *ateenvv1.GetProcessRequest) (*ateenvv1.Process, error) {
	if req.GetProcessId() == "" {
		return nil, status.Error(codes.InvalidArgument, "process_id cannot be empty")
	}

	state, ok := s.tracker.Get(req.GetProcessId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %q not found", req.GetProcessId())
	}

	return state.ToProto(), nil
}

// StreamProcessLogs streams stdout and stderr logs in real-time or as a snapshot.
func (s *Service) StreamProcessLogs(req *ateenvv1.StreamProcessLogsRequest, stream ateenvv1.ProcessService_StreamProcessLogsServer) error {
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
			return status.Errorf(codes.Internal, "reading stdout logs: %v", err)
		}
		if len(stdoutBytes) > 0 {
			if err := stream.Send(&ateenvv1.ProcessLogChunk{
				Source: ateenvv1.LogSource_LOG_SOURCE_STDOUT,
				Data:   stdoutBytes,
			}); err != nil {
				return err
			}
			stdoutOffset = newStdoutOffset
		}

		// Read stderr delta
		stderrBytes, newStderrOffset, err := ReadLogs(state.StderrPath, stderrOffset)
		if err != nil {
			return status.Errorf(codes.Internal, "reading stderr logs: %v", err)
		}
		if len(stderrBytes) > 0 {
			if err := stream.Send(&ateenvv1.ProcessLogChunk{
				Source: ateenvv1.LogSource_LOG_SOURCE_STDERR,
				Data:   stderrBytes,
			}); err != nil {
				return err
			}
			stderrOffset = newStderrOffset
		}

		if !follow {
			// Snapshot mode: finish after reading available logs up to this point
			return nil
		}

		// Check if process has finished and we consumed all logs
		state.mu.RLock()
		isTerminated := state.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING
		state.mu.RUnlock()

		if isTerminated {
			// Final check to see if there were any remaining bytes flushed on exit
			finalStdout, _, _ := ReadLogs(state.StdoutPath, stdoutOffset)
			if len(finalStdout) > 0 {
				_ = stream.Send(&ateenvv1.ProcessLogChunk{
					Source: ateenvv1.LogSource_LOG_SOURCE_STDOUT,
					Data:   finalStdout,
				})
			}
			finalStderr, _, _ := ReadLogs(state.StderrPath, stderrOffset)
			if len(finalStderr) > 0 {
				_ = stream.Send(&ateenvv1.ProcessLogChunk{
					Source: ateenvv1.LogSource_LOG_SOURCE_STDERR,
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
func (s *Service) KillProcess(ctx context.Context, req *ateenvv1.KillProcessRequest) (*ateenvv1.KillProcessResponse, error) {
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

	return &ateenvv1.KillProcessResponse{
		ExitCode: exitCode,
	}, nil
}
