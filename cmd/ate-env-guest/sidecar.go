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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sidecars are extra runtimes started next to the guest, from the same
// container: typically a second data-plane daemon that was mounted into the
// actor as an image volume (see docs/task-images/RUNTIMES.md). The guest is
// the container's only process by contract, so it is the one that starts
// them, restarts them if they exit, and folds their readiness into /readyz.

// multiFlag collects the values of a repeatable flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, "; ") }

func (m *multiFlag) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("value must not be empty")
	}
	*m = append(*m, v)
	return nil
}

// parseSidecar turns a -sidecar value into argv. Values are split on
// whitespace and no quoting is interpreted: sidecars run without a shell, so
// a runtime that needs one ships a wrapper script in its layer. The binary
// must be an absolute path, which is what a mounted layer provides and what
// keeps the guest independent of the task image's PATH.
func parseSidecar(v string) ([]string, error) {
	argv := strings.Fields(v)
	if len(argv) == 0 {
		return nil, errors.New("sidecar command is empty")
	}
	if !filepath.IsAbs(argv[0]) {
		return nil, fmt.Errorf("sidecar %q: the binary must be an absolute path (sidecars run without a shell or PATH lookup)", argv[0])
	}
	return argv, nil
}

const (
	sidecarMinBackoff = time.Second
	sidecarMaxBackoff = 30 * time.Second
	// sidecarStableAfter is how long a sidecar must have run for its next
	// restart to start from the minimum backoff again.
	sidecarStableAfter = time.Minute
)

// runSidecar runs argv until ctx is done, restarting it whenever it exits.
// The backoff doubles from one second to thirty while the sidecar keeps
// failing quickly and resets once it has run for a while. Output goes to
// logf, one line at a time, prefixed with the binary's name.
func runSidecar(ctx context.Context, argv []string, logf func(string, ...any)) {
	name := filepath.Base(argv[0])
	backoff := sidecarMinBackoff
	for {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = os.Environ()
		out := &lineLogger{prefix: name, logf: logf}
		cmd.Stdout = out
		cmd.Stderr = out
		start := time.Now()
		err := cmd.Run()
		out.flush()
		if ctx.Err() != nil {
			return
		}
		ran := time.Since(start)
		logf("sidecar %s exited after %s: %v; restarting in %s", name, ran.Round(time.Millisecond), err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if ran >= sidecarStableAfter {
			backoff = sidecarMinBackoff
		} else {
			backoff = min(backoff*2, sidecarMaxBackoff)
		}
	}
}

// lineLogger writes complete lines to logf with a prefix; a partial trailing
// line is kept until the next write or flush.
type lineLogger struct {
	prefix string
	logf   func(string, ...any)
	mu     sync.Mutex
	buf    bytes.Buffer
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	for {
		line, err := l.buf.ReadString('\n')
		if err != nil {
			// No newline yet: put the partial line back.
			l.buf.Reset()
			l.buf.WriteString(line)
			break
		}
		l.logf("[%s] %s", l.prefix, strings.TrimRight(line, "\r\n"))
	}
	return len(p), nil
}

func (l *lineLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len() > 0 {
		l.logf("[%s] %s", l.prefix, l.buf.String())
		l.buf.Reset()
	}
}

// readyGate makes /readyz wait for the sidecars' own readiness endpoints.
// Each URL is polled until it answers 2xx once; after that it is not asked
// again. Readiness is a start-up gate, so Substrate's wakeup probe admits
// traffic only when every runtime in the actor is serving, not a liveness
// check on the sidecars.
type readyGate struct {
	urls   []string
	client *http.Client

	mu   sync.Mutex
	seen map[string]bool
}

func newReadyGate(urls []string) *readyGate {
	return &readyGate{
		urls:   urls,
		client: &http.Client{Timeout: 2 * time.Second},
		seen:   make(map[string]bool, len(urls)),
	}
}

// Ready reports whether every URL has answered 2xx, and if not, which one is
// still pending and why.
func (g *readyGate) Ready(ctx context.Context) (bool, string) {
	for _, u := range g.urls {
		g.mu.Lock()
		ok := g.seen[u]
		g.mu.Unlock()
		if ok {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return false, fmt.Sprintf("sidecar readiness %s: %v", u, err)
		}
		resp, err := g.client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("sidecar readiness %s: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return false, fmt.Sprintf("sidecar readiness %s: HTTP %d", u, resp.StatusCode)
		}
		g.mu.Lock()
		g.seen[u] = true
		g.mu.Unlock()
	}
	return true, ""
}
