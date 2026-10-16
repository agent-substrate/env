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
	"strings"
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

// An environment that stays suspended past the wake budget fails with
// Unavailable, naming the state and the attempt count.
func TestGuestCallFailsWhenWakeBudgetExpires(t *testing.T) {
	te := newFullTestEnv(t)
	envCtx := registerGuest(t, te, "budget-test")
	te.control.SetState("budget-test", ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
	te.srv.WakeBudget = 1500 * time.Millisecond

	start := time.Now()
	_, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", "true"}})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code %v (%v), want Unavailable", status.Code(err), err)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "did not answer within") || !strings.Contains(msg, "SUSPENDED") {
		t.Errorf("message %q should name the budget and the state", msg)
	}
	if elapsed := time.Since(start); elapsed < 1400*time.Millisecond || elapsed > 6*time.Second {
		t.Errorf("gave up after %s, want about the 1.5 s budget", elapsed)
	}
}

// An environment the control plane reports RUNNING, whose guest the router
// still cannot reach, fails after the shorter running budget rather than
// holding the call for the full wake budget.
func TestGuestCallCapsWaitWhenRunning(t *testing.T) {
	te := newFullTestEnv(t)
	envCtx := registerGuest(t, te, "running-test")
	te.control.SetState("running-test", ateapipb.ActorState_ACTOR_STATE_RUNNING)
	te.router.Running = func(string) bool { return false } // path down: 503 regardless of state
	te.srv.WakeBudget = time.Minute
	te.srv.WakeRunningBudget = time.Second

	start := time.Now()
	_, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", "true"}})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code %v (%v), want Unavailable", status.Code(err), err)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "RUNNING") {
		t.Errorf("message %q should name the RUNNING state", msg)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s; the running budget (1 s) must apply, not the wake budget (1 min)", elapsed)
	}
}

// A wake whose suspended phase outlasts the running budget must still get
// the running budget once the actor is RUNNING: the guest listens a moment
// after the state flips.
func TestGuestCallRunningBudgetStartsAtRunning(t *testing.T) {
	te := newFullTestEnv(t)
	envCtx := registerGuest(t, te, "late-running-test")
	te.control.SetState("late-running-test", ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
	te.srv.WakeBudget = time.Minute
	te.srv.WakeRunningBudget = 3 * time.Second

	start := time.Now()
	flip := start.Add(2500 * time.Millisecond)   // the control plane reports RUNNING; the next attempt is at ~3.0 s
	listen := start.Add(4500 * time.Millisecond) // the guest answers 2 s later, inside the 3 s running budget
	te.router.Running = func(string) bool { return time.Now().After(listen) }
	go func() {
		time.Sleep(time.Until(flip))
		te.control.SetState("late-running-test", ateapipb.ActorState_ACTOR_STATE_RUNNING)
	}()

	_, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", "true"}})
	if err != nil {
		t.Fatalf("StartProcess after a 2.5 s suspended phase and a 2 s listen lag: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 4400*time.Millisecond {
		t.Errorf("call returned after %s, before the guest was listening", elapsed)
	}
}
