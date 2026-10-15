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
	"fmt"
	"io/fs"
	"time"

	env "github.com/agent-substrate/env/clients/go"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
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
type EnvBackend struct {
	client       *env.Client
	atespace     string
	baseTemplate string
}

// NewEnvBackend dials the ate-env API. Environments are created in atespace
// from baseTemplate with the task image as rootfs; the API derives and bakes
// one template per distinct image on first use.
func NewEnvBackend(endpoint, atespace, baseTemplate string) (*EnvBackend, error) {
	c, err := env.NewClient(env.ClientOptions{Endpoint: endpoint})
	if err != nil {
		return nil, err
	}
	return &EnvBackend{client: c, atespace: atespace, baseTemplate: baseTemplate}, nil
}

// Close releases the API connection.
func (b *EnvBackend) Close() error { return b.client.Close() }

// Create implements Backend.
func (b *EnvBackend) Create(ctx context.Context, id, image string) (Sandbox, error) {
	e, err := b.client.Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
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

func (s envSandbox) Run(ctx context.Context, command string, timeout time.Duration) (Result, error) {
	resp, err := s.e.Run(ctx, env.ShellRequest{Command: command, Timeout: timeout})
	if err != nil {
		return Result{}, err
	}
	return Result{ExitCode: resp.ExitCode, Stdout: resp.Stdout, Stderr: resp.Stderr}, nil
}

func (s envSandbox) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error {
	return s.e.WriteFile(ctx, path, bytes.NewReader(content), mode)
}

func (s envSandbox) Suspend(ctx context.Context) error { return s.e.Suspend(ctx) }

func (s envSandbox) Delete(ctx context.Context) error { return s.e.Delete(ctx) }
