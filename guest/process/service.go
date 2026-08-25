package process

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MaxPollWait caps StreamProcessLogs's wait_ms long-poll. It stays below
// the atenet router's route timeout (10s by default) so a held poll always
// completes as a response rather than tripping the router's deadline.
const MaxPollWait = 5 * time.Second

// logChunkBytes caps one ProcessLogChunk message, keeping every message
// comfortably under gRPC's default 4 MiB receive ceiling even when the
// spooled logs are larger; bigger deltas stream as several chunks.
const logChunkBytes int64 = 1 << 20

// Service implements ateenvv1.ProcessServiceServer.
// It provides in-actor asynchronous process execution and log streaming for cmd/ate-env-guest.
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

	state, err := s.tracker.Start(req.GetCommand(), req.GetCwd(), req.GetEnv(), req.GetStdin())
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

// WriteStdin forwards data to a process's standard input.
func (s *Service) WriteStdin(ctx context.Context, req *ateenvv1.WriteStdinRequest) (*ateenvv1.WriteStdinResponse, error) {
	if req.GetProcessId() == "" {
		return nil, status.Error(codes.InvalidArgument, "process_id cannot be empty")
	}
	if err := s.tracker.WriteStdin(ctx, req.GetProcessId(), req.GetData(), req.GetClose()); err != nil {
		return nil, err
	}
	return &ateenvv1.WriteStdinResponse{}, nil
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

	// Without follow, wait_ms turns the snapshot into a bounded long-poll:
	// hold until there is something to send or the process exits, at most
	// wait_ms (server-capped below the router's route timeout).
	wait := min(time.Duration(req.GetWaitMs())*time.Millisecond, MaxPollWait)
	deadline := time.Now().Add(wait)

	for {
		// Read the status before draining: when the drain runs at or
		// after termination, it is guaranteed to include everything the
		// exiting process flushed (the reaper syncs the log files before
		// flipping the status).
		state.mu.RLock()
		isTerminated := state.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING
		state.mu.RUnlock()

		sent := false

		// Drain the stdout delta in bounded chunks.
		for {
			stdoutBytes, newStdoutOffset, err := ReadLogs(state.StdoutPath, stdoutOffset, logChunkBytes)
			if err != nil {
				return status.Errorf(codes.Internal, "reading stdout logs: %v", err)
			}
			if len(stdoutBytes) == 0 {
				break
			}
			if err := stream.Send(&ateenvv1.ProcessLogChunk{
				Source: ateenvv1.LogSource_LOG_SOURCE_STDOUT,
				Data:   stdoutBytes,
			}); err != nil {
				return err
			}
			stdoutOffset = newStdoutOffset
			sent = true
		}

		// Drain the stderr delta in bounded chunks.
		for {
			stderrBytes, newStderrOffset, err := ReadLogs(state.StderrPath, stderrOffset, logChunkBytes)
			if err != nil {
				return status.Errorf(codes.Internal, "reading stderr logs: %v", err)
			}
			if len(stderrBytes) == 0 {
				break
			}
			if err := stream.Send(&ateenvv1.ProcessLogChunk{
				Source: ateenvv1.LogSource_LOG_SOURCE_STDERR,
				Data:   stderrBytes,
			}); err != nil {
				return err
			}
			stderrOffset = newStderrOffset
			sent = true
		}

		if !follow {
			// Snapshot mode: return once something was sent, the process
			// is done, or the long-poll window closed.
			if sent || isTerminated || !time.Now().Before(deadline) {
				return nil
			}
		} else if isTerminated {
			// The drain above ran after the status flipped, so everything
			// flushed on exit has been sent.
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

// KillProcess signals a running process (SIGKILL unless the request names
// another signal) and returns its exit code when it has one.
func (s *Service) KillProcess(ctx context.Context, req *ateenvv1.KillProcessRequest) (*ateenvv1.KillProcessResponse, error) {
	if req.GetProcessId() == "" {
		return nil, status.Error(codes.InvalidArgument, "process_id cannot be empty")
	}

	sig := syscall.SIGKILL
	if req.GetSignal() != 0 {
		sig = syscall.Signal(req.GetSignal())
	}

	exitCode, err := s.tracker.Signal(req.GetProcessId(), sig)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.NotFound, "process %q not found", req.GetProcessId())
		}
		return nil, err
	}

	return &ateenvv1.KillProcessResponse{
		ExitCode: exitCode,
	}, nil
}
