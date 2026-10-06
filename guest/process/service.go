// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package process

import (
	"context"
	"errors"
	"io"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements ateenvv1alpha.ProcessServiceServer on top of a Tracker.
type Service struct {
	ateenvv1alpha.UnimplementedProcessServiceServer
	tracker *Tracker
}

// NewService creates a new ProcessServiceServer instance.
func NewService(tracker *Tracker) *Service {
	return &Service{tracker: tracker}
}

// StartProcess launches a process in the background and returns its resource.
func (s *Service) StartProcess(ctx context.Context, req *ateenvv1alpha.StartProcessRequest) (*ateenvv1alpha.Process, error) {
	if len(req.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command cannot be empty")
	}
	if req.GetTimeout() != nil && !req.GetTimeout().IsValid() {
		return nil, status.Error(codes.InvalidArgument, "timeout is invalid")
	}

	state, err := s.tracker.Start(StartOptions{
		Command: req.GetCommand(),
		Cwd:     req.GetCwd(),
		Env:     req.GetEnv(),
		Stdin:   req.GetStdin(),
		Timeout: req.GetTimeout().AsDuration(),
	})
	if err != nil {
		return nil, rpcError(err, "")
	}
	return state.ToProto(), nil
}

// GetProcess returns the current state of a process.
func (s *Service) GetProcess(ctx context.Context, req *ateenvv1alpha.GetProcessRequest) (*ateenvv1alpha.Process, error) {
	state, err := s.lookup(req.GetProcessId())
	if err != nil {
		return nil, err
	}
	return state.ToProto(), nil
}

// StreamProcessOutput streams stdout and stderr, ending with an exit message
// once the process has exited and its output has been fully delivered.
func (s *Service) StreamProcessOutput(req *ateenvv1alpha.StreamProcessOutputRequest, stream grpc.ServerStreamingServer[ateenvv1alpha.ProcessOutput]) error {
	state, err := s.lookup(req.GetProcessId())
	if err != nil {
		return err
	}
	if req.GetStdoutOffset() < 0 || req.GetStderrOffset() < 0 {
		return status.Error(codes.InvalidArgument, "offsets cannot be negative")
	}

	ctx := stream.Context()
	cursor := &outputCursor{
		state:  state,
		stream: stream,
		stdout: req.GetStdoutOffset(),
		stderr: req.GetStderrOffset(),
	}

	for {
		// Grab the change signal before reading so a write that lands between
		// the read and the select still wakes us.
		changed := state.OutputChanged()
		if err := cursor.flush(); err != nil {
			return err
		}

		if state.Exited() {
			// The reaper flushed the spool before marking the process exited;
			// pick up anything written after our last read, then finish.
			if err := cursor.flush(); err != nil {
				return err
			}
			return stream.Send(&ateenvv1alpha.ProcessOutput{
				Output: &ateenvv1alpha.ProcessOutput_Exit{Exit: state.ToProto()},
			})
		}
		if !req.GetFollow() {
			return nil
		}

		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-changed:
		case <-state.Done():
		}
	}
}

// outputCursor tracks read offsets into a process's spool files and sends deltas.
type outputCursor struct {
	state  *ProcessState
	stream grpc.ServerStreamingServer[ateenvv1alpha.ProcessOutput]
	stdout int64
	stderr int64
}

func (c *outputCursor) flush() error {
	data, next, err := ReadSpool(c.state.StdoutPath, c.stdout)
	if err != nil {
		return status.Errorf(codes.Internal, "reading stdout: %v", err)
	}
	if len(data) > 0 {
		if err := c.stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Stdout{Stdout: data}}); err != nil {
			return err
		}
	}
	c.stdout = next

	data, next, err = ReadSpool(c.state.StderrPath, c.stderr)
	if err != nil {
		return status.Errorf(codes.Internal, "reading stderr: %v", err)
	}
	if len(data) > 0 {
		if err := c.stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Stderr{Stderr: data}}); err != nil {
			return err
		}
	}
	c.stderr = next
	return nil
}

// WriteProcessInput feeds stdin from a client stream. stdin stays open across
// calls until a message sets close.
func (s *Service) WriteProcessInput(stream grpc.ClientStreamingServer[ateenvv1alpha.WriteProcessInputRequest, ateenvv1alpha.WriteProcessInputResponse]) error {
	var (
		processID string
		total     int64
	)
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if processID == "" {
			processID = req.GetProcessId()
			if processID == "" {
				return status.Error(codes.InvalidArgument, "process_id is required on the first message")
			}
			if _, err := s.tracker.Get(processID); err != nil {
				return rpcError(err, processID)
			}
		}
		if len(req.GetData()) > 0 {
			n, err := s.tracker.WriteInput(processID, req.GetData())
			total += int64(n)
			if err != nil {
				return rpcError(err, processID)
			}
		}
		if req.GetClose() {
			if err := s.tracker.CloseInput(processID); err != nil {
				return rpcError(err, processID)
			}
		}
	}
	if processID == "" {
		return status.Error(codes.InvalidArgument, "process_id is required on the first message")
	}
	return stream.SendAndClose(&ateenvv1alpha.WriteProcessInputResponse{BytesWritten: total})
}

// SignalProcess delivers a signal to the process group and returns the
// process state right after delivery.
func (s *Service) SignalProcess(ctx context.Context, req *ateenvv1alpha.SignalProcessRequest) (*ateenvv1alpha.Process, error) {
	state, err := s.lookup(req.GetProcessId())
	if err != nil {
		return nil, err
	}
	sig, ok := ToSyscallSignal(req.GetSignal())
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported signal %v", req.GetSignal())
	}
	if err := s.tracker.Signal(req.GetProcessId(), sig); err != nil {
		return nil, rpcError(err, req.GetProcessId())
	}
	return state.ToProto(), nil
}

func (s *Service) lookup(processID string) (*ProcessState, error) {
	if processID == "" {
		return nil, status.Error(codes.InvalidArgument, "process_id cannot be empty")
	}
	state, err := s.tracker.Get(processID)
	if err != nil {
		return nil, rpcError(err, processID)
	}
	return state, nil
}

// rpcError maps Tracker errors to gRPC statuses.
func rpcError(err error, processID string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return status.Errorf(codes.NotFound, "process %q not found", processID)
	case errors.Is(err, ErrExited):
		return status.Errorf(codes.FailedPrecondition, "process %q has exited", processID)
	case errors.Is(err, ErrNoStdin):
		return status.Errorf(codes.FailedPrecondition, "process %q was started without stdin", processID)
	case errors.Is(err, ErrStdinClosed):
		return status.Errorf(codes.FailedPrecondition, "stdin of process %q is closed", processID)
	case errors.Is(err, ErrTooManyProcesses):
		return status.Errorf(codes.ResourceExhausted, "%v; wait for running processes to exit or signal them", err)
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}
