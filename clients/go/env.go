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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Env is a handle to a single environment.
type Env struct {
	id       string
	atespace string
	client   *Client
}

// ID returns the environment's identifier.
func (e *Env) ID() string { return e.id }

func (e *Env) withEnv(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-env-id", e.id, "x-env-atespace", e.atespace)
}

// Suspend checkpoints and stops the environment using gRPC.
func (e *Env) Suspend(ctx context.Context) error {
	return e.client.Suspend(ctx, e.atespace, e.id)
}

// Delete removes the environment permanently using gRPC.
func (e *Env) Delete(ctx context.Context) error {
	return e.client.Delete(ctx, e.atespace, e.id)
}

// Shell runs a shell command line inside the environment and captures its
// output. It is shorthand for Run with only Command set.
func (e *Env) Shell(ctx context.Context, commandLine string) (*ShellResponse, error) {
	return e.Run(ctx, ShellRequest{Command: commandLine})
}

// Run executes `sh -c req.Command` inside the environment, feeds it
// req.Stdin, and returns its buffered output and exit status once it exits.
// For incremental output or interactive input use StartProcess.
func (e *Env) Run(ctx context.Context, req ShellRequest) (*ShellResponse, error) {
	start := &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sh", "-c", req.Command},
		Cwd:     req.Cwd,
		Env:     req.Env,
		Stdin:   req.Stdin != nil,
	}
	if req.Timeout > 0 {
		start.Timeout = durationpb.New(req.Timeout)
	}
	proc, err := e.StartProcess(ctx, start)
	if err != nil {
		return nil, err
	}

	if req.Stdin != nil {
		w, err := proc.Stdin(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(req.Stdin); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	}

	stream, err := proc.Output(ctx, &ateenvv1alpha.StreamProcessOutputRequest{Follow: true})
	if err != nil {
		return nil, err
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	var exit *ateenvv1alpha.Process
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fromGRPCError(err)
		}
		switch out := msg.GetOutput().(type) {
		case *ateenvv1alpha.ProcessOutput_Stdout:
			stdoutBuf.Write(out.Stdout)
		case *ateenvv1alpha.ProcessOutput_Stderr:
			stderrBuf.Write(out.Stderr)
		case *ateenvv1alpha.ProcessOutput_Exit:
			exit = out.Exit
		}
	}
	if exit == nil {
		return nil, errors.New("env: output stream ended before the process exited")
	}

	return &ShellResponse{
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
		ExitCode: int(exit.GetExitCode()),
	}, nil
}

// ReadFile streams the contents of the file at path inside the environment using FileSystemService.
// The caller must close the returned reader.
func (e *Env) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	ctx = e.withEnv(ctx)
	stream, err := e.client.filesystem.ReadFile(ctx, &ateenvv1alpha.ReadFileRequest{
		Path: p,
	})
	if err != nil {
		return nil, fromGRPCError(err)
	}

	// Read the first chunk eagerly to check for immediate errors (e.g. NotFound, PermissionDenied)
	firstChunk, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		// Empty file
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if err != nil {
		return nil, fromGRPCError(err)
	}

	pr, pw := io.Pipe()
	go func() {
		if len(firstChunk.GetChunk()) > 0 {
			if _, writeErr := pw.Write(firstChunk.GetChunk()); writeErr != nil {
				return
			}
		}
		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				_ = pw.Close()
				return
			}
			if err != nil {
				_ = pw.CloseWithError(fromGRPCError(err))
				return
			}
			if len(chunk.GetChunk()) > 0 {
				if _, writeErr := pw.Write(chunk.GetChunk()); writeErr != nil {
					return
				}
			}
		}
	}()

	return pr, nil
}

// WriteFile replaces the file at path inside the environment with the
// contents of r, creating it with the given permissions if needed.
func (e *Env) WriteFile(ctx context.Context, p string, r io.Reader, mode fs.FileMode) error {
	return e.writeFile(ctx, p, r, mode, 0)
}

// WriteFileAt writes the contents of r into the file at path starting at
// byte offset, keeping existing content outside the written range. The file
// is created with the given permissions if it does not exist, and extended
// with zero bytes if offset is past its end. An offset of zero replaces the
// file, like WriteFile.
func (e *Env) WriteFileAt(ctx context.Context, p string, offset int64, r io.Reader, mode fs.FileMode) error {
	if offset < 0 {
		return fmt.Errorf("env: negative offset %d for %q", offset, p)
	}
	return e.writeFile(ctx, p, r, mode, offset)
}

func (e *Env) writeFile(ctx context.Context, p string, r io.Reader, mode fs.FileMode, seekOffset int64) error {
	ctx = e.withEnv(ctx)
	stream, err := e.client.filesystem.WriteFile(ctx)
	if err != nil {
		return fromGRPCError(err)
	}

	buf := make([]byte, 64*1024)
	n, readErr := r.Read(buf)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return fmt.Errorf("env: reading data for %q: %w", p, readErr)
	}

	firstReq := &ateenvv1alpha.WriteFileRequest{
		Path:       p,
		Mode:       uint32(mode.Perm()),
		Chunk:      buf[:n],
		SeekOffset: seekOffset,
	}
	if err := stream.Send(firstReq); err != nil {
		return fromGRPCError(err)
	}

	if !errors.Is(readErr, io.EOF) {
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&ateenvv1alpha.WriteFileRequest{Chunk: buf[:n]}); sendErr != nil {
					return fromGRPCError(sendErr)
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("env: reading data for %q: %w", p, err)
			}
		}
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		return fromGRPCError(err)
	}
	return nil
}
