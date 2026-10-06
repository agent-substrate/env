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

package env

import (
	"context"
	"errors"
	"io"
	"math"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// Process is a handle to a process inside an environment. Process state is
// reported with the ateenvv1alpha.Process message; its exit_code follows the
// shell convention (128 + signal number when killed by a signal).
type Process struct {
	id  string
	env *Env
}

// ID returns the process identifier.
func (p *Process) ID() string { return p.id }

// StartProcess launches a process and returns a handle to it.
func (e *Env) StartProcess(ctx context.Context, req *ateenvv1alpha.StartProcessRequest) (*Process, error) {
	pb, err := e.client.process.StartProcess(e.withEnv(ctx), req)
	if err != nil {
		return nil, fromGRPCError(err)
	}
	return e.Process(pb.GetProcessId()), nil
}

// Process returns a handle to an existing process without checking that it exists.
func (e *Env) Process(id string) *Process {
	return &Process{id: id, env: e}
}

// FindProcess returns the current state of the process with the given ID,
// or an error wrapping ErrNotFound if the guest no longer tracks it.
func (e *Env) FindProcess(ctx context.Context, id string) (*ateenvv1alpha.Process, error) {
	pb, err := e.client.process.GetProcess(e.withEnv(ctx), &ateenvv1alpha.GetProcessRequest{ProcessId: id})
	if err != nil {
		return nil, fromGRPCError(err)
	}
	return pb, nil
}

// Wait blocks until the process exits or ctx is done, and returns its final
// state. It follows the output stream with offsets past the end of the
// spool, so no output is transferred.
func (p *Process) Wait(ctx context.Context) (*ateenvv1alpha.Process, error) {
	stream, err := p.Output(ctx, &ateenvv1alpha.StreamProcessOutputRequest{
		Follow:       true,
		StdoutOffset: math.MaxInt64,
		StderrOffset: math.MaxInt64,
	})
	if err != nil {
		return nil, err
	}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("env: output stream ended before the process exited")
		}
		if err != nil {
			return nil, fromGRPCError(err)
		}
		if exit := msg.GetExit(); exit != nil {
			return exit, nil
		}
	}
}

// Signal delivers sig to the process and its process group.
func (p *Process) Signal(ctx context.Context, sig ateenvv1alpha.Signal) error {
	_, err := p.env.client.process.SignalProcess(p.env.withEnv(ctx), &ateenvv1alpha.SignalProcessRequest{ProcessId: p.id, Signal: sig})
	return fromGRPCError(err)
}

// Kill sends SIGKILL and waits for the process to exit. Killing a process
// that has already exited is not an error.
func (p *Process) Kill(ctx context.Context) (*ateenvv1alpha.Process, error) {
	if err := p.Signal(ctx, ateenvv1alpha.Signal_SIGNAL_KILL); err != nil && !errors.Is(err, ErrProcessExited) {
		return nil, err
	}
	return p.Wait(ctx)
}

// Output streams the process's stdout and stderr as ateenvv1alpha.ProcessOutput
// messages. req may be nil for a snapshot of the output so far; its
// process_id is always set to this process. With follow, the stream ends
// with an exit message once the process has exited and all output has been
// delivered. Cancel ctx to stop early.
func (p *Process) Output(ctx context.Context, req *ateenvv1alpha.StreamProcessOutputRequest) (grpc.ServerStreamingClient[ateenvv1alpha.ProcessOutput], error) {
	if req == nil {
		req = &ateenvv1alpha.StreamProcessOutputRequest{}
	} else {
		req = proto.Clone(req).(*ateenvv1alpha.StreamProcessOutputRequest)
	}
	req.ProcessId = p.id
	stream, err := p.env.client.process.StreamProcessOutput(p.env.withEnv(ctx), req)
	if err != nil {
		return nil, fromGRPCError(err)
	}
	return stream, nil
}

// Stdin opens a writer to the process's standard input. Writes are delivered
// as they happen; Close sends EOF. The process must have been started with
// stdin enabled. Errors from the guest surface on Write or Close.
func (p *Process) Stdin(ctx context.Context) (io.WriteCloser, error) {
	stream, err := p.env.client.process.WriteProcessInput(p.env.withEnv(ctx))
	if err != nil {
		return nil, fromGRPCError(err)
	}
	return &stdinWriter{stream: stream, id: p.id}, nil
}

type stdinWriter struct {
	stream ateenvv1alpha.ProcessService_WriteProcessInputClient
	id     string
	sent   bool
	closed bool
}

func (w *stdinWriter) Write(data []byte) (int, error) {
	if w.closed {
		return 0, errors.New("env: stdin is closed")
	}
	for i := 0; i < len(data); i += chunkSize {
		end := min(i+chunkSize, len(data))
		if err := w.send(&ateenvv1alpha.WriteProcessInputRequest{Data: data[i:end]}); err != nil {
			return i, err
		}
	}
	return len(data), nil
}

// Close signals EOF on stdin and waits for the guest to acknowledge.
func (w *stdinWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.send(&ateenvv1alpha.WriteProcessInputRequest{Close: true}); err != nil {
		return err
	}
	_, err := w.stream.CloseAndRecv()
	return fromGRPCError(err)
}

// send forwards one message, resolving the guest's real error when the
// stream has already been torn down (grpc reports that as io.EOF on Send).
func (w *stdinWriter) send(req *ateenvv1alpha.WriteProcessInputRequest) error {
	if !w.sent {
		req.ProcessId = w.id
		w.sent = true
	}
	if err := w.stream.Send(req); err != nil {
		w.closed = true
		if errors.Is(err, io.EOF) {
			_, err = w.stream.CloseAndRecv()
		}
		return fromGRPCError(err)
	}
	return nil
}
