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

import "testing"

func TestEnvBackendSpreadsConnections(t *testing.T) {
	// grpc.NewClient does not dial until the first call, so a backend over
	// an unreachable address is enough to check the pool and the rotation.
	b, err := NewEnvBackend("127.0.0.1:1", "ate-env", "default-template", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(b.clients) != 3 {
		t.Fatalf("%d clients, want 3", len(b.clients))
	}
	seen := map[int]int{}
	for i := 0; i < 9; i++ {
		c := b.pick()
		for j, cc := range b.clients {
			if cc == c {
				seen[j]++
			}
		}
	}
	for j := 0; j < 3; j++ {
		if seen[j] != 3 {
			t.Fatalf("connection %d picked %d times, want 3: %v", j, seen[j], seen)
		}
	}
	one, err := NewEnvBackend("127.0.0.1:1", "ate-env", "default-template", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	if len(one.clients) != 1 {
		t.Fatalf("connections 0 must mean 1, got %d", len(one.clients))
	}
}
