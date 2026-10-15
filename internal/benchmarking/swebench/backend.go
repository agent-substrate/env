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

package swebench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync/atomic"
	"time"

	env "github.com/agent-substrate/env/clients/go"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Result is what a Run step returned.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Sandbox is the subset of an environment the loader drives.
type Sandbox interface {
	Run(ctx context.Context, command string, timeout time.Duration) (Result, error)
	WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error
	Suspend(ctx context.Context) error
	Delete(ctx context.Context) error
}

// Backend creates sandboxes. The loader is written against this interface so
// tests run it against an in-memory fake and a second SDK or backend can be
// compared with the same workload.
type Backend interface {
	Create(ctx context.Context, id, image string) (Sandbox, error)
}

// EnvBackend is the Backend over the ate-env API through the Go SDK.
//
// It holds several connections and hands them out round-robin per
// environment: a Kubernetes Service balances per connection, so one
// connection would put every task on one API replica, and gRPC caps the
// concurrent streams on a connection.
type EnvBackend struct {
	clients      []*env.Client
	next         atomic.Uint64
	atespace     string
	baseTemplate string
}

// NewEnvBackend dials the ate-env API over connections connections (at
// least one). Environments are created in atespace from baseTemplate with
// the task image as rootfs; the API derives and bakes one template per
// distinct image on first use.
func NewEnvBackend(endpoint, atespace, baseTemplate string, connections int) (*EnvBackend, error) {
	if connections < 1 {
		connections = 1
	}
	b := &EnvBackend{atespace: atespace, baseTemplate: baseTemplate}
	for i := 0; i < connections; i++ {
		c, err := env.NewClient(env.ClientOptions{Endpoint: endpoint})
		if err != nil {
			b.Close()
			return nil, err
		}
		b.clients = append(b.clients, c)
	}
	return b, nil
}

// Close releases the API connections.
func (b *EnvBackend) Close() error {
	var first error
	for _, c := range b.clients {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// pick returns the next connection round-robin.
func (b *EnvBackend) pick() *env.Client {
	return b.clients[int(b.next.Add(1)-1)%len(b.clients)]
}

// Create implements Backend. The environment keeps the connection it was
// created on for the rest of its life.
func (b *EnvBackend) Create(ctx context.Context, id, image string) (Sandbox, error) {
	e, err := b.pick().Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
		Id:          id,
		Atespace:    b.atespace,
		Template:    &ateenvv1alpha.Template{Name: b.baseTemplate, Atespace: b.atespace},
		RootfsImage: image,
	})
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", id, err)
	}
	return envSandbox{e: e}, nil
}

type envSandbox struct {
	e *env.Env
}

// Polling bounds for a running process: short at first so quick commands
// return quickly, backing off so a long test run does not hammer the API.
const (
	pollMin = 50 * time.Millisecond
	pollMax = 2 * time.Second
	// pollRetries is how many consecutive transient poll errors are
	// tolerated before the step fails; a wake or an API pod restart costs a
	// few.
	pollRetries = 10
)

// Run starts the command, polls until it exits and then reads its output.
// The SDK's streaming Shell holds one request open through the router for
// the whole command, and the router's route timeout (five minutes by
// default) cuts it off; SWE-bench test runs regularly take longer. Start,
// poll and read are each short calls, so no single request can outlive it.
func (s envSandbox) Run(ctx context.Context, command string, timeout time.Duration) (Result, error) {
	start := &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", command}}
	if timeout > 0 {
		start.Timeout = durationpb.New(timeout)
	}
	proc, err := s.e.StartProcess(ctx, start)
	if err != nil {
		return Result{}, err
	}

	wait := pollMin
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(wait):
		}
		p, err := s.e.FindProcess(ctx, proc.ID())
		switch {
		case err == nil:
			failures = 0
			if p.GetState() == ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
				return s.collect(ctx, proc, int(p.GetExitCode()))
			}
		case status.Code(err) == codes.Unavailable || status.Code(err) == codes.DeadlineExceeded:
			failures++
			if failures > pollRetries {
				return Result{}, fmt.Errorf("poll %s: %w", proc.ID(), err)
			}
		default:
			return Result{}, err
		}
		if wait < pollMax {
			wait = min(wait*2, pollMax)
		}
	}
}

// collect reads the exited process's buffered output in one non-following
// call.
func (s envSandbox) collect(ctx context.Context, proc *env.Process, exitCode int) (Result, error) {
	stream, err := proc.Output(ctx, &ateenvv1alpha.StreamProcessOutputRequest{})
	if err != nil {
		return Result{}, err
	}
	var stdout, stderr bytes.Buffer
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, err
		}
		switch out := msg.GetOutput().(type) {
		case *ateenvv1alpha.ProcessOutput_Stdout:
			stdout.Write(out.Stdout)
		case *ateenvv1alpha.ProcessOutput_Stderr:
			stderr.Write(out.Stderr)
		case *ateenvv1alpha.ProcessOutput_Exit:
			exitCode = int(out.Exit.GetExitCode())
		}
	}
	return Result{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

func (s envSandbox) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error {
	return s.e.WriteFile(ctx, path, bytes.NewReader(content), mode)
}

func (s envSandbox) Suspend(ctx context.Context) error { return s.e.Suspend(ctx) }

func (s envSandbox) Delete(ctx context.Context) error { return s.e.Delete(ctx) }
