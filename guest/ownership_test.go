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

package guest

import (
	"context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"strconv"
	"testing"
	"time"
)

func ownerContext(generation int, token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(OwnerGenerationHeader, strconv.Itoa(generation), OwnerTokenHeader, token))
}
func TestOwnerHandoffCancelsCallsBeforeDrainAndRejectsStaleOwners(t *testing.T) {
	drains := 0
	fence := &ownerFence{drain: func() error { drains++; return nil }}
	old, release, err := fence.acquire(ownerContext(1, "old"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, release, err := fence.acquire(ownerContext(2, "new"))
		if release != nil {
			release()
		}
		done <- err
	}()
	select {
	case <-old.Done():
	case <-time.After(time.Second):
		t.Fatal("old call not canceled")
	}
	if drains != 1 {
		t.Fatal("drained while old call still active")
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("handoff did not finish")
	}
	if drains != 2 {
		t.Fatal("handoff did not drain")
	}
	for _, ctx := range []context.Context{ownerContext(1, "old"), ownerContext(2, "wrong"), context.Background()} {
		_, _, err := fence.acquire(ctx)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("stale owner admitted: %v", err)
		}
	}
}
