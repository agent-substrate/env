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

package portproxy_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/portproxy"
)

// recordingRouter wraps the fake router and records each CONNECT's actor
// reference and authority.
type recordingRouter struct {
	*fakerouter.Router
	mu       sync.Mutex
	connects []string
}

func (r *recordingRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		r.mu.Lock()
		r.connects = append(r.connects, req.Header.Get(ate.TargetActorHeader)+" "+req.Host)
		r.mu.Unlock()
	}
	r.Router.ServeHTTP(w, req)
}

func (r *recordingRouter) connectCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.connects)
}

type setup struct {
	router     *recordingRouter
	routerAddr string
	proxy      *httptest.Server
}

func newSetup(t *testing.T) *setup {
	t.Helper()
	rr := &recordingRouter{Router: fakerouter.New()}
	srv := httptest.NewServer(rr)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	p := portproxy.New(portproxy.Options{
		ConnectAddr:      addr,
		DefaultAtespace:  "ate-env",
		HandshakeTimeout: 5 * time.Second,
	})
	proxy := httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	return &setup{router: rr, routerAddr: addr, proxy: proxy}
}

func (s *setup) do(t *testing.T, method, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, s.proxy.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func target(id string, port int) map[string]string {
	return map[string]string{portproxy.EnvIDHeader: id, portproxy.EnvPortHeader: fmt.Sprint(port)}
}

func TestForwardsToEnvironmentPort(t *testing.T) {
	s := newSetup(t)
	var seen *http.Request
	var seenBody string
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("X-Daemon", "yes")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "daemon says hi")
	}))

	h := target("env1", 8000)
	h["X-Custom"] = "kept"
	resp, body := s.do(t, http.MethodPost, "/execute?lang=python", `{"code":"1+1"}`, h)

	if resp.StatusCode != http.StatusCreated || body != "daemon says hi" || resp.Header.Get("X-Daemon") != "yes" {
		t.Fatalf("got %d %q (X-Daemon %q), want 201 from the daemon", resp.StatusCode, body, resp.Header.Get("X-Daemon"))
	}
	if seen.Method != http.MethodPost || seen.URL.RequestURI() != "/execute?lang=python" || seenBody != `{"code":"1+1"}` {
		t.Errorf("daemon saw %s %s %q", seen.Method, seen.URL.RequestURI(), seenBody)
	}
	if seen.Host != "127.0.0.1:8000" {
		t.Errorf("daemon saw Host %q, want 127.0.0.1:8000 (what an in-sandbox client sends)", seen.Host)
	}
	for _, hdr := range []string{portproxy.EnvIDHeader, portproxy.EnvAtespaceHeader, portproxy.EnvPortHeader} {
		if v := seen.Header.Get(hdr); v != "" {
			t.Errorf("daemon saw routing header %s=%q; the proxy must strip it", hdr, v)
		}
	}
	if seen.Header.Get("X-Custom") != "kept" {
		t.Error("other request headers must reach the daemon")
	}
	if got := s.router.connects; len(got) != 1 || got[0] != "ate-env/env1 env1."+ate.DefaultHostSuffix+":8000" {
		t.Errorf("router saw CONNECTs %q, want one for ate-env/env1 on port 8000", got)
	}
}

func TestAtespaceHeaderNamesTheActor(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("env1", 9000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h := target("env1", 9000)
	h[portproxy.EnvAtespaceHeader] = "team-a"
	if resp, body := s.do(t, http.MethodGet, "/", "", h); resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if got := s.router.connects; len(got) != 1 || !strings.HasPrefix(got[0], "team-a/env1 ") {
		t.Errorf("router saw %q, want the actor team-a/env1", got)
	}
}

func TestStreamsResponses(t *testing.T) {
	s := newSetup(t)
	release := make(chan struct{})
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "second\n")
	}))
	req, _ := http.NewRequest(http.MethodGet, s.proxy.URL+"/events", nil)
	for k, v := range target("env1", 8000) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, len("first\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "first\n" {
		t.Fatalf("first chunk %q, %v: the proxy must flush before the daemon finishes", buf, err)
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "second\n" {
		t.Errorf("rest %q", rest)
	}
}

func TestRejectsBadTargets(t *testing.T) {
	s := newSetup(t)
	for name, h := range map[string]map[string]string{
		"no id":           {portproxy.EnvPortHeader: "8000"},
		"port not number": {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "http"},
		"port zero":       {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "0"},
		"port too large":  {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "70000"},
		"id uppercase":    {portproxy.EnvIDHeader: "Env1", portproxy.EnvPortHeader: "8000"},
		"id with slash":   {portproxy.EnvIDHeader: "a/b", portproxy.EnvPortHeader: "8000"},
		"bad atespace":    {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "8000", portproxy.EnvAtespaceHeader: "x y"},
	} {
		if resp, body := s.do(t, http.MethodGet, "/", "", h); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d %q, want 400", name, resp.StatusCode, body)
		}
	}
	if n := s.router.connectCount(); n != 0 {
		t.Errorf("router saw %d CONNECTs for rejected requests, want 0", n)
	}
}

func TestPassesRouterRefusals(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("asleep", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Running = func(id string) bool { return id != "asleep" }

	if resp, body := s.do(t, http.MethodGet, "/", "", target("missing", 8000)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown environment: got %d %q, want 404", resp.StatusCode, body)
	}
	if resp, body := s.do(t, http.MethodGet, "/", "", target("asleep", 8000)); resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "ate-env/asleep port 8000") {
		t.Errorf("not running: got %d %q, want 503 naming the target", resp.StatusCode, body)
	}
}

func TestPoolsConnectionsPerTarget(t *testing.T) {
	s := newSetup(t)
	for _, id := range []string{"env1", "env2"} {
		id := id
		s.router.RegisterPort(id, 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, id) }))
	}
	for i := 0; i < 3; i++ {
		for _, id := range []string{"env1", "env2"} {
			if _, body := s.do(t, http.MethodGet, "/", "", target(id, 8000)); body != id {
				t.Fatalf("request for %s answered by %q: tunnels must not be shared across environments", id, body)
			}
		}
	}
	if n := s.router.connectCount(); n != 2 {
		t.Errorf("router saw %d CONNECTs for 6 sequential requests to 2 targets, want 2 (idle tunnels reused)", n)
	}
}

func TestDispatch(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "proxied") }))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api") })
	h := portproxy.Dispatch(portproxy.New(portproxy.Options{ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env"}), next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Body.String() != "api" {
		t.Errorf("request without %s went to %q, want the API", portproxy.EnvPortHeader, rec.Body.String())
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	for k, v := range target("env1", 8000) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "proxied" {
		t.Errorf("request with %s got %q, want the daemon's answer", portproxy.EnvPortHeader, b)
	}
}
