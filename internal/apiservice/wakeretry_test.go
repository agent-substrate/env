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

package apiservice_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/apiservice"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// registerGuest creates an environment and serves a real guest for it on the
// fake router, returning the metadata context that addresses it.
func registerGuest(t *testing.T, te *testEnv, id string) context.Context {
	t.Helper()
	ctx := context.Background()
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: id}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	srv, cleanup, err := guest.NewServer(guest.Config{Workspace: t.TempDir(), EnableProcess: true, EnableFileSystem: true})
	if err != nil {
		t.Fatalf("guest.NewServer: %v", err)
	}
	t.Cleanup(cleanup)
	te.router.Register(id, srv)
	return metadata.AppendToOutgoingContext(ctx, "x-env-id", id, "x-env-atespace", apiservice.DefaultAtespace)
}

// A guest call made while the environment is still suspended is held until
// the environment is running, instead of failing with the router's 503.
func TestGuestCallWaitsForWake(t *testing.T) {
	te := newFullTestEnv(t)
	envCtx := registerGuest(t, te, "wake-test")
	te.control.SetState("wake-test", ateapipb.ActorState_ACTOR_STATE_SUSPENDED)

	// The fake router has no resume path of its own; stand in for it.
	go func() {
		time.Sleep(600 * time.Millisecond)
		te.control.SetState("wake-test", ateapipb.ActorState_ACTOR_STATE_RUNNING)
	}()

	start := time.Now()
	proc, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", "echo awake"}})
	if err != nil {
		t.Fatalf("StartProcess while suspended: %v", err)
	}
	if proc.GetProcessId() == "" {
		t.Fatal("StartProcess returned no process id")
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("call returned after %s, before the environment was running", elapsed)
	}
}

// A crashed environment fails the call at once rather than consuming the
// wake budget.
func TestGuestCallFailsFastOnCrash(t *testing.T) {
	te := newFullTestEnv(t)
	envCtx := registerGuest(t, te, "crash-test")
	te.control.SetState("crash-test", ateapipb.ActorState_ACTOR_STATE_CRASHED)

	start := time.Now()
	_, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", "true"}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("StartProcess on a crashed environment: code %v (%v), want FailedPrecondition", status.Code(err), err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("crash took %s to report; the wake budget must not apply", elapsed)
	}
}
