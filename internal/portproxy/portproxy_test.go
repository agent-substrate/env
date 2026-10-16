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
	"bufio"
	"fmt"
	"io"
	"net"
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
	p := mustNew(t, portproxy.Options{
		ConnectAddr:      addr,
		DefaultAtespace:  "ate-env",
		HandshakeTimeout: 5 * time.Second,
		WakeBudget:       2 * time.Second,
	})
	proxy := httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	return &setup{router: rr, routerAddr: addr, proxy: proxy}
}

// mustNew builds a proxy and drops its idle tunnels when the test ends, so
// the fake router's tunnel goroutines end with the test.
func mustNew(t *testing.T, opts portproxy.Options) *portproxy.Proxy {
	t.Helper()
	p, err := portproxy.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
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
		"port with sign":  {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "+8000"},
		"port zero-led":   {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "08000"},
		"bad atespace":    {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "8000", portproxy.EnvAtespaceHeader: "x y"},
		"guest port":      {portproxy.EnvIDHeader: "env1", portproxy.EnvPortHeader: "80"},
	} {
		if resp, body := s.do(t, http.MethodGet, "/", "", h); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d %q, want 400", name, resp.StatusCode, body)
		}
	}
	// A routing header sent twice is ambiguous.
	req, _ := http.NewRequest(http.MethodGet, s.proxy.URL+"/", nil)
	req.Header.Add(portproxy.EnvIDHeader, "env1")
	req.Header.Add(portproxy.EnvIDHeader, "env2")
	req.Header.Set(portproxy.EnvPortHeader, "8000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("duplicate %s: got %d, want 400", portproxy.EnvIDHeader, resp.StatusCode)
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

// A suspended environment answers 503 at the router until it is back; the
// proxy tries again within its wake budget instead of passing the first 503
// on, the way ate-env-api's guest calls do.
func TestRetriesWhileTheEnvironmentWakes(t *testing.T) {
	s := newSetup(t)
	s.router.RegisterPort("waking", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "awake") }))
	var mu sync.Mutex
	refusals := 0
	s.router.Running = func(id string) bool {
		mu.Lock()
		defer mu.Unlock()
		if refusals < 2 {
			refusals++
			return false
		}
		return true
	}
	resp, body := s.do(t, http.MethodGet, "/", "", target("waking", 8000))
	if resp.StatusCode != http.StatusOK || body != "awake" {
		t.Fatalf("got %d %q, want the daemon's answer after the wake", resp.StatusCode, body)
	}
	if n := s.router.connectCount(); n != 3 {
		t.Errorf("router saw %d CONNECTs, want 3 (two refused while waking, one accepted)", n)
	}
}

func TestWakeBudgetBoundsTheWait(t *testing.T) {
	s := newSetup(t)
	p := mustNew(t, portproxy.Options{ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second, WakeBudget: 300 * time.Millisecond})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	s.router.RegisterPort("asleep", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.router.Running = func(string) bool { return false }
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	for k, v := range target("asleep", 8000) {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), "not available") {
		t.Errorf("got %d %q, want 503 once the wake budget is spent", resp.StatusCode, b)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 3*time.Second {
		t.Errorf("waited %s, want about the 300ms budget", took)
	}
	if n := s.router.connectCount(); n < 2 {
		t.Errorf("router saw %d CONNECTs, want at least 2 (the budget allows retries)", n)
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
	h := portproxy.Dispatch(mustNew(t, portproxy.Options{ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env"}), next)

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

	// A gRPC call is never proxied, even with the header: the tunnel carries
	// HTTP/1.1 only.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/ateenv.v1alpha.EnvironmentService/Get", nil)
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set(portproxy.EnvPortHeader, "8000")
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "api" {
		t.Errorf("gRPC request with %s went to %q, want the API", portproxy.EnvPortHeader, rec.Body.String())
	}
}

// pathSetup serves the proxy the way ate-env-api does: both forms dispatched
// in front of the API's mux.
func pathSetup(t *testing.T, origins ...string) (*setup, string) {
	t.Helper()
	s := newSetup(t)
	p := mustNew(t, portproxy.Options{ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env", HandshakeTimeout: 5 * time.Second, TrustedOrigins: origins})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api") })
	srv := httptest.NewServer(portproxy.Dispatch(p, mux))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func TestPathFormStripsPrefix(t *testing.T) {
	s, base := pathSetup(t)
	var seen *http.Request
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		io.WriteString(w, "daemon")
	}))
	resp, err := http.Get(base + "/v1alpha/envs/env1/ports/8000/api/file/download?path=/etc/os-release")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "daemon" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
	if got := seen.URL.RequestURI(); got != "/api/file/download?path=/etc/os-release" {
		t.Errorf("daemon saw %q, want the path after the routing prefix with its query", got)
	}
	if got := s.router.connects; len(got) != 1 || !strings.HasPrefix(got[0], "ate-env/env1 ") {
		t.Errorf("router saw %q", got)
	}

	// Escaping after the prefix is kept as sent.
	resp, err = http.Get(base + "/v1alpha/envs/env1/ports/8000/files/a%2Fb")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := seen.URL.EscapedPath(); got != "/files/a%2Fb" {
		t.Errorf("daemon saw escaped path %q, want /files/a%%2Fb", got)
	}

	// The bare prefix with a trailing slash is the daemon's root.
	resp, err = http.Get(base + "/v1alpha/envs/env1/ports/8000/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen.URL.Path != "/" {
		t.Errorf("daemon saw %q for the bare prefix, want /", seen.URL.Path)
	}
}

func TestPathFormAtespaceAndValidation(t *testing.T) {
	s, base := pathSetup(t)
	s.router.RegisterPort("env1", 9000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.URL.Path) }))
	resp, err := http.Get(base + "/v1alpha/atespaces/team-a/envs/env1/ports/9000/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "/x" {
		t.Errorf("got %d %q, want the daemon's /x", resp.StatusCode, b)
	}
	if got := s.router.connects; len(got) != 1 || !strings.HasPrefix(got[0], "team-a/env1 ") {
		t.Errorf("router saw %q, want team-a/env1", got)
	}
	for _, path := range []string{
		"/v1alpha/envs/Env1/ports/9000/x",
		"/v1alpha/envs/env1/ports/0/x",
		"/v1alpha/envs/env1/ports/http/x",
		"/v1alpha/envs/env1/ports/09000/x",
		"/v1alpha/envs//ports/9000/x",
		"/v1alpha/envs/a%2Fb/ports/9000/x",
		"/v1alpha/atespaces/A_B/envs/env1/ports/9000/x",
	} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", path, resp.StatusCode)
		}
	}
	// Other paths still reach the API.
	resp, err = http.Get(base + "/v1alpha/envs/env1/mcp")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "api" {
		t.Errorf("/v1alpha/envs/env1/mcp went to %q, want the API", b)
	}
}

// An HTTP upgrade (what a WebSocket handshake is) passes through the proxy and
// the tunnel, after which bytes flow both ways.
func TestUpgradePassesThrough(t *testing.T) {
	s, base := pathSetup(t)
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "want Upgrade: echo", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err == nil {
			rw.Write(append([]byte("re:"), buf...))
			rw.Flush()
		}
	}))
	addr := strings.TrimPrefix(base, "http://")
	for name, req := range map[string]string{
		"path form":   "GET /v1alpha/envs/env1/ports/8000/sockets/events/c1 HTTP/1.1\r\nHost: " + addr + "\r\n",
		"header form": "GET /sockets/events/c1 HTTP/1.1\r\nHost: " + addr + "\r\nX-Env-Id: env1\r\nX-Env-Port: 8000\r\n",
	} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprint(conn, req+"Connection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("%s: upgrade answered %v, %v", name, resp, err)
		}
		conn.Write([]byte("ping"))
		got := make([]byte, 7)
		if _, err := io.ReadFull(br, got); err != nil || string(got) != "re:ping" {
			t.Errorf("%s: after upgrade got %q, %v; want re:ping", name, got, err)
		}
		conn.Close()
	}
}

// The path form reaches the daemon exactly as sent: an escaped routing segment
// still ends the prefix, and nothing is redirected or cleaned.
func TestPathFormKeepsPathAsSent(t *testing.T) {
	s, base := pathSetup(t)
	var seen *http.Request
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		io.WriteString(w, "daemon")
	}))
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, c := range []struct{ method, path, want string }{
		{http.MethodGet, "/v1alpha/envs/%65nv1/ports/8000/a", "/a"},
		{http.MethodGet, "/v1alpha/envs/env1/ports/%38000/a", "/a"},
		{http.MethodGet, "/v1alpha/envs/env1/ports/8000", "/"},
		{http.MethodPost, "/v1alpha/envs/env1/ports/8000", "/"},
		{http.MethodPost, "/v1alpha/atespaces/ate-env/envs/env1/ports/8000", "/"},
		{http.MethodGet, "/v1alpha/envs/env1/ports/8000/a//b/../c?x=1", "/a//b/../c?x=1"},
	} {
		seen = nil
		req, _ := http.NewRequest(c.method, base+c.path, nil)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(b) != "daemon" || seen == nil {
			t.Errorf("%s %s: got %d %q, want the daemon's 200", c.method, c.path, resp.StatusCode, b)
			continue
		}
		if got := seen.URL.RequestURI(); got != c.want {
			t.Errorf("%s %s: daemon saw %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// A browser page from another origin cannot drive a daemon through the path
// form: no state-changing request, no WebSocket handshake. Reads, non-browser
// clients, the listener's own origin and trusted origins pass.
func TestPathFormRefusesCrossOrigin(t *testing.T) {
	s, base := pathSetup(t, "https://ui.example")
	s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "echo")
			w.WriteHeader(http.StatusSwitchingProtocols)
			return
		}
		io.WriteString(w, "daemon")
	}))
	self := base // the listener's own origin, as a browser on it sends
	path := base + "/v1alpha/envs/env1/ports/8000/run"
	for _, c := range []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"cross-site POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"foreign Origin POST", http.MethodPost, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"foreign Origin upgrade", http.MethodGet, map[string]string{"Origin": "https://evil.example", "Connection": "Upgrade", "Upgrade": "echo"}, http.StatusForbidden},
		{"cross-site upgrade", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site", "Connection": "Upgrade", "Upgrade": "echo"}, http.StatusForbidden},
		{"cross-site GET", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusOK},
		{"no browser headers", http.MethodPost, nil, http.StatusOK},
		{"own origin POST", http.MethodPost, map[string]string{"Origin": self}, http.StatusOK},
		{"same-origin upgrade", http.MethodGet, map[string]string{"Sec-Fetch-Site": "same-origin", "Connection": "Upgrade", "Upgrade": "echo"}, http.StatusSwitchingProtocols},
		{"trusted origin POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://ui.example"}, http.StatusOK},
	} {
		req, _ := http.NewRequest(c.method, path, nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: got %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	if _, err := portproxy.New(portproxy.Options{ConnectAddr: s.routerAddr, TrustedOrigins: []string{"not an origin"}}); err == nil {
		t.Error("New accepted a malformed trusted origin")
	}
}

// An error body names the target and a short cause. The router's message and
// address stay in the log.
func TestErrorBodiesNameCauseOnly(t *testing.T) {
	refuser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "dial tcp 10.1.2.3:8000: connect: connection refused", http.StatusBadGateway)
	}))
	defer refuser.Close()
	p := mustNew(t, portproxy.Options{ConnectAddr: strings.TrimPrefix(refuser.URL, "http://"), DefaultAtespace: "ate-env"})
	srv := httptest.NewServer(p)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	for k, v := range target("env1", 8000) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "ate-env/env1 port 8000") || !strings.Contains(body, "0.0.0.0") {
		t.Errorf("got %d %q, want 502 naming the target and the listen rule", resp.StatusCode, body)
	}
	if strings.Contains(body, "10.1.2.3") || strings.Contains(body, refuser.URL[len("http://"):]) {
		t.Errorf("body %q leaks an internal address", body)
	}
}

func TestResponseHeaderTimeout(t *testing.T) {
	rr := &recordingRouter{Router: fakerouter.New()}
	router := httptest.NewServer(rr)
	defer router.Close()
	release := make(chan struct{})
	defer close(release)
	rr.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	p := mustNew(t, portproxy.Options{
		ConnectAddr:           strings.TrimPrefix(router.URL, "http://"),
		DefaultAtespace:       "ate-env",
		ResponseHeaderTimeout: 200 * time.Millisecond,
	})
	srv := httptest.NewServer(p)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	for k, v := range target("env1", 8000) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second} // fail, not hang, without the limit
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("silent daemon: got %d, want 504", resp.StatusCode)
	}
}
