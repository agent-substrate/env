package env

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc/metadata"
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

// Shell runs a shell command line inside the environment using ProcessService.
func (e *Env) Shell(ctx context.Context, commandLine string) (*ShellResponse, error) {
	ctx = e.withEnv(ctx)
	startResp, err := e.client.process.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", commandLine},
	})
	if err != nil {
		return nil, fromGRPCError(err)
	}

	pid := startResp.GetProcessId()
	outStream, err := e.client.process.StreamProcessOutputs(ctx, &ateenvv1.StreamProcessOutputsRequest{
		ProcessId: pid,
		Follow:    true,
	})
	if err != nil {
		return nil, fromGRPCError(err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	for {
		chunk, err := outStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fromGRPCError(err)
		}
		switch chunk.GetSource() {
		case ateenvv1.OutputSource_OUTPUT_SOURCE_STDOUT:
			stdoutBuf.Write(chunk.GetData())
		case ateenvv1.OutputSource_OUTPUT_SOURCE_STDERR:
			stderrBuf.Write(chunk.GetData())
		}
	}

	// Retrieve final process state / exit code
	var exitCode int
	for {
		proc, err := e.client.process.GetProcess(ctx, &ateenvv1.GetProcessRequest{
			ProcessId: pid,
		})
		if err != nil {
			return nil, fromGRPCError(err)
		}
		if proc.GetStatus() != ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING {
			exitCode = int(proc.GetExitCode())
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}

	return &ShellResponse{
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
		ExitCode: exitCode,
	}, nil
}

// ReadFile streams the contents of the file at path inside the environment using FileSystemService.
// The caller must close the returned reader.
func (e *Env) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	ctx = e.withEnv(ctx)
	stream, err := e.client.filesystem.ReadFile(ctx, &ateenvv1.ReadFileRequest{
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
		if len(firstChunk.GetData()) > 0 {
			if _, writeErr := pw.Write(firstChunk.GetData()); writeErr != nil {
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
			if len(chunk.GetData()) > 0 {
				if _, writeErr := pw.Write(chunk.GetData()); writeErr != nil {
					return
				}
			}
		}
	}()

	return pr, nil
}

// WriteFile streams the contents of r to the file at path inside the
// environment with the given permissions using FileSystemService.
func (e *Env) WriteFile(ctx context.Context, p string, r io.Reader, mode fs.FileMode) error {
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

	firstReq := &ateenvv1.WriteFileRequest{
		Path:  p,
		Mode:  uint32(mode.Perm()),
		Chunk: buf[:n],
	}
	if err := stream.Send(firstReq); err != nil {
		return fromGRPCError(err)
	}

	if !errors.Is(readErr, io.EOF) {
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&ateenvv1.WriteFileRequest{Chunk: buf[:n]}); sendErr != nil {
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
