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
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DefaultWakeBudget bounds how long a guest call waits for its environment
// to wake before the error reaches the caller. The first request that
// reaches the router is what resumes a suspended actor, and the router holds
// that request only for its own parking budget, a few seconds. A wake that
// has to pull the environment's image onto the node or read its snapshot
// across a region takes longer than that: 18 s for a cold image pull and 7 s
// for a 1 GiB snapshot read across regions, measured on GKE.
const DefaultWakeBudget = 2 * time.Minute

const (
	wakeRetryInitialBackoff = 200 * time.Millisecond
	wakeRetryMaxBackoff     = 2 * time.Second
)

// ensureAwake sends a trivial request to the environment's guest over conn
// and repeats it while the router answers Unavailable, which is what an
// expired parking budget and a guest that is not yet listening both look
// like. It returns when the guest answers anything else, when the actor has
// crashed, when ctx ends, or when the wake budget is spent. Because the probe
// is the first request through the router, it is also what wakes the actor;
// the calls that follow on conn find it up.
func (s *Server) ensureAwake(ctx context.Context, conn *grpc.ClientConn, atespace, id string) error {
	budget := s.WakeBudget
	if budget <= 0 {
		budget = DefaultWakeBudget
	}
	deadline := time.Now().Add(budget)
	probe := ateenvv1alpha.NewProcessServiceClient(conn)
	backoff := wakeRetryInitialBackoff
	for attempt := 1; ; attempt++ {
		_, err := probe.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: "ate-env-wake-probe"})
		if status.Code(err) != codes.Unavailable {
			// Anything else, NotFound for the probe id included, came from
			// the guest itself.
			return nil
		}
		crashed, cerr := s.actorCrashed(ctx, atespace, id)
		if cerr != nil {
			return cerr
		}
		if crashed {
			return status.Errorf(codes.FailedPrecondition, "environment %s/%s crashed while waking: %v", atespace, id, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return status.Errorf(codes.Unavailable, "environment %s/%s did not wake within %s (%d attempts): %v", atespace, id, budget, attempt, err)
		}
		if attempt == 1 {
			log.Printf("waiting for environment %s/%s to wake (budget %s)", atespace, id, budget)
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(min(backoff, remaining)):
		}
		backoff = min(backoff*2, wakeRetryMaxBackoff)
	}
}

// actorCrashed reports whether the control plane has the actor in a crashed
// state, in which case waiting for a wake is pointless.
func (s *Server) actorCrashed(ctx context.Context, atespace, id string) (bool, error) {
	actor, err := s.client.Get(ctx, atespace, id)
	if err != nil {
		return false, toGRPCError(err)
	}
	return actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED, nil
}
