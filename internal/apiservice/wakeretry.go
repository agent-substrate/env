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

package apiservice

import (
	"context"
	"log"
	"strings"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DefaultWakeBudget bounds how long a guest call waits for an environment
// that the control plane reports as suspended or in transition (resuming,
// pausing, suspending) to wake before the error reaches the caller. The first
// request that reaches the router is what resumes a suspended actor, and the
// router holds that request only for its own parking budget, a few seconds. A
// wake that has to pull the environment's image onto the node or read its
// snapshot across a region takes longer than that: 18 s for a cold image pull
// and 7 s for a 1 GiB snapshot read across regions, measured on GKE.
const DefaultWakeBudget = 2 * time.Minute

// DefaultWakeRunningBudget bounds the wait when the control plane already
// reports the environment RUNNING but the router still answers Unavailable.
// That has two causes: the guest is not yet listening after a restore, which
// resolves in seconds, or the path to the worker is down, which does not.
// A short budget covers the first without turning the second into a
// two-minute hang per call.
const DefaultWakeRunningBudget = 20 * time.Second

const (
	wakeRetryInitialBackoff = 200 * time.Millisecond
	wakeRetryMaxBackoff     = 2 * time.Second
)

// ensureAwake sends a trivial request to the environment's guest over conn
// and repeats it while the router answers Unavailable, which is what an
// expired parking budget and a guest that is not yet listening both look
// like. It returns when the guest answers anything else, when the actor has
// crashed, when ctx ends, or when a budget is spent: WakeBudget bounds the
// whole wait, and WakeRunningBudget bounds the part of it that starts with
// the first attempt that sees the actor RUNNING, so a long suspended phase
// does not eat the time the guest needs to start listening after the state
// flips. Because the probe is the first request through the router, it is
// also what wakes the actor; the calls that follow on conn find it up. The
// state is read once per attempt, and the backoff caps attempts at roughly
// one per two seconds, so a full two-minute wait costs about sixty
// control-plane reads.
func (s *Server) ensureAwake(ctx context.Context, conn *grpc.ClientConn, atespace, id string) error {
	budget := s.WakeBudget
	if budget <= 0 {
		budget = DefaultWakeBudget
	}
	runningBudget := s.WakeRunningBudget
	if runningBudget <= 0 {
		runningBudget = DefaultWakeRunningBudget
	}
	start := time.Now()
	var runningSince time.Time // first attempt that saw the actor RUNNING
	probe := ateenvv1alpha.NewProcessServiceClient(conn)
	backoff := wakeRetryInitialBackoff
	for attempt := 1; ; attempt++ {
		_, err := probe.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: "ate-env-wake-probe"})
		if status.Code(err) != codes.Unavailable {
			// Anything else, NotFound for the probe id included, came from
			// the guest itself.
			return nil
		}
		state, serr := s.actorState(ctx, atespace, id)
		if serr != nil {
			return serr
		}
		if state == ateapipb.ActorState_ACTOR_STATE_CRASHED {
			return status.Errorf(codes.FailedPrecondition, "environment %s/%s crashed while waking: %v", atespace, id, err)
		}
		allowed, since, which := budget, start, "wake budget"
		if state == ateapipb.ActorState_ACTOR_STATE_RUNNING {
			if runningSince.IsZero() {
				runningSince = time.Now()
			}
			allowed, since, which = runningBudget, runningSince, "running budget"
		} else {
			runningSince = time.Time{}
		}
		remaining := allowed - time.Since(since)
		if overall := budget - time.Since(start); overall < remaining {
			remaining, allowed, which = overall, budget, "wake budget"
		}
		if remaining <= 0 {
			return status.Errorf(codes.Unavailable, "environment %s/%s did not answer within the %s of %s while %s (%d attempts): %v",
				atespace, id, which, allowed, shortState(state), attempt, err)
		}
		if attempt == 1 {
			log.Printf("waiting for environment %s/%s to wake (state %s, %s %s)", atespace, id, shortState(state), which, allowed)
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(min(backoff, remaining)):
		}
		backoff = min(backoff*2, wakeRetryMaxBackoff)
	}
}

// actorState reads the actor's state from the control plane. A deleted
// environment surfaces here as NotFound (through toGRPCError), which is the
// right answer to a caller still holding its handle.
func (s *Server) actorState(ctx context.Context, atespace, id string) (ateapipb.ActorState, error) {
	actor, err := s.client.Get(ctx, atespace, id)
	if err != nil {
		return ateapipb.ActorState_ACTOR_STATE_UNSPECIFIED, toGRPCError(err)
	}
	return actor.GetStatus().GetState(), nil
}

func shortState(st ateapipb.ActorState) string {
	return strings.TrimPrefix(st.String(), "ACTOR_STATE_")
}
