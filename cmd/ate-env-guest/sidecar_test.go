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

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSidecar(t *testing.T) {
	argv, err := parseSidecar("  /opt/osb/execd --port 44772  ")
	if err != nil || strings.Join(argv, " ") != "/opt/osb/execd --port 44772" {
		t.Errorf("parseSidecar = %v, %v", argv, err)
	}
	for _, bad := range []string{"", "   ", "execd --port 1", "sh -c /opt/osb/execd"} {
		if _, err := parseSidecar(bad); err == nil {
			t.Errorf("parseSidecar(%q) accepted a non-absolute or empty command", bad)
		}
	}
	var m multiFlag
	if err := m.Set(""); err == nil {
		t.Error("empty flag value accepted")
	}
	_ = m.Set("/a")
	_ = m.Set("/b x")
	if len(m) != 2 || m.String() != "/a; /b x" {
		t.Errorf("multiFlag = %v", m)
	}
}

func TestReadyGateWaitsOnceForEachURL(t *testing.T) {
	var state atomic.Int32 // 0: 503, 1: 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if state.Load() == 0 {
			http.Error(w, "booting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	g := newReadyGate([]string{srv.URL + "/ready"})
	ctx := context.Background()
	if ok, why := g.Ready(ctx); ok || !strings.Contains(why, "HTTP 503") {
		t.Errorf("not ready expected, got ok=%v why=%q", ok, why)
	}
	state.Store(1)
	if ok, why := g.Ready(ctx); !ok {
		t.Errorf("ready expected, got %q", why)
	}
	// Once seen, the URL is not re-checked: readiness is a start-up gate.
	state.Store(0)
	if ok, _ := g.Ready(ctx); !ok {
		t.Error("readiness flipped back after the sidecar had answered once")
	}

	unreachable := newReadyGate([]string{closedURL + "/ready"})
	if ok, why := unreachable.Ready(ctx); ok || why == "" {
		t.Errorf("an unreachable sidecar must report not ready with a reason, got ok=%v why=%q", ok, why)
	}
	if ok, _ := newReadyGate(nil).Ready(ctx); !ok {
		t.Error("no sidecars means ready")
	}
}

func TestRunSidecarRestartsAndStopsWithContext(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this host")
	}
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	// A sidecar that prints and exits non-zero is restarted with backoff.
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		runSidecar(ctx, []string{sh, "-c", "echo hello from sidecar; exit 3"}, logf)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSidecar did not return after its context ended")
	}
	mu.Lock()
	joined := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "[sh] hello from sidecar") {
		t.Errorf("sidecar output was not logged with its prefix:\n%s", joined)
	}
	if n := strings.Count(joined, "restarting in"); n < 2 {
		t.Errorf("expected at least two restarts within the window (1s then 2s backoff), got %d:\n%s", n, joined)
	}
	if !strings.Contains(joined, "restarting in 1s") || !strings.Contains(joined, "restarting in 2s") {
		t.Errorf("backoff did not double:\n%s", joined)
	}

	// A long-running sidecar is killed promptly when the guest shuts down.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		runSidecar(ctx2, []string{sh, "-c", "sleep 60"}, logf)
		close(done2)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel2()
	select {
	case <-done2:
	case <-time.After(3 * time.Second):
		t.Fatal("sidecar was not stopped when the context was cancelled")
	}
}
