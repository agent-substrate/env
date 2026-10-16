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

// Package fakerouter implements a test double for the atenet router: it
// resolves the target environment from the request's ate-target-actor header
// (or, failing that, its Host header) and forwards to that environment's guest
// handler, returning 404 for an invalid or duplicated actor reference and 503
// when the environment is not running. Like the real router's CONNECT
// listener, it also tunnels an HTTP CONNECT whose authority names a port to
// the handler registered for that environment and port.
package fakerouter

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/agent-substrate/env/internal/ate"
)

// Router is a fake atenet router.
type Router struct {
	// Running reports whether the actor with the given ID is currently
	// running. Non-running actors get a 503.
	Running func(id string) bool

	mu     sync.Mutex
	guests map[string]http.Handler
	ports  map[portKey]http.Handler
}

type portKey struct {
	id   string
	port int
}

// New returns a Router that considers every actor running unless a Running
// callback is set.
func New() *Router {
	return &Router{guests: make(map[string]http.Handler), ports: make(map[portKey]http.Handler)}
}

// RegisterPort installs the handler that serves CONNECT tunnels to port on
// environment id, standing in for a daemon listening there.
func (r *Router) RegisterPort(id string, port int, h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ports[portKey{id, port}] = h
}

// Register installs the guest handler serving a environment ID.
func (r *Router) Register(id string, h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guests[id] = h
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id, ok := actorID(req)
	if !ok {
		http.Error(w, "invalid actor reference", http.StatusNotFound)
		return
	}
	if req.Method == http.MethodConnect {
		r.serveConnect(w, req, id)
		return
	}
	r.mu.Lock()
	guest := r.guests[id]
	r.mu.Unlock()
	if guest == nil {
		http.Error(w, "no actor "+id, http.StatusNotFound)
		return
	}
	if r.Running != nil && !r.Running(id) {
		http.Error(w, "actor "+id+" is not running", http.StatusServiceUnavailable)
		return
	}
	guest.ServeHTTP(w, req)
}

func actorID(req *http.Request) (string, bool) {
	// Like the real router since agent-substrate/substrate#2308: a request
	// carrying ate-target-actor more than once is an invalid actor reference,
	// even when every copy agrees.
	values := req.Header.Values(ate.TargetActorHeader)
	if len(values) > 1 {
		return "", false
	}
	if len(values) == 1 && values[0] != "" {
		_, actor, ok := strings.Cut(values[0], "/")
		return actor, ok && actor != ""
	}
	if !strings.HasSuffix(req.Host, "."+ate.DefaultHostSuffix) {
		return "", false
	}
	id, _, ok := strings.Cut(req.Host, ".")
	return id, ok && id != ""
}

// Serve starts the router on a random localhost port and returns its
// address and a shutdown function.
func (r *Router) Serve() (addr string, stop func()) {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := httptest.NewUnstartedServer(r)
	srv.Config.Protocols = &protocols
	srv.Start()
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}

// serveConnect answers a CONNECT like the real router: the actor comes from
// ate-target-actor, the port from the authority. On success the hijacked
// connection carries HTTP to the registered port handler.
func (r *Router) serveConnect(w http.ResponseWriter, req *http.Request, id string) {
	_, portStr, err := net.SplitHostPort(req.Host)
	port, perr := strconv.Atoi(portStr)
	if err != nil || perr != nil {
		http.Error(w, "CONNECT authority names no port", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	h := r.ports[portKey{id, port}]
	r.mu.Unlock()
	if h == nil {
		http.Error(w, "no actor "+id+" listening on "+portStr, http.StatusNotFound)
		return
	}
	if r.Running != nil && !r.Running(id) {
		http.Error(w, "actor "+id+" is not running", http.StatusServiceUnavailable)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		conn.Close()
		return
	}
	l := &oneConnListener{conn: &tunnelConn{Conn: conn, r: rw.Reader}, done: make(chan struct{})}
	_ = (&http.Server{Handler: h}).Serve(l)
}

// tunnelConn reads what the hijacked reader buffered before the connection.
type tunnelConn struct {
	net.Conn
	r       *bufio.Reader
	closeFn func()
}

func (c *tunnelConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (c *tunnelConn) Close() error {
	if c.closeFn != nil {
		c.closeFn()
	}
	return c.Conn.Close()
}

// oneConnListener hands out one connection, then blocks until it is closed.
type oneConnListener struct {
	conn   *tunnelConn
	once   sync.Once
	closed sync.Once
	done   chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() {
		l.conn.closeFn = func() { l.closed.Do(func() { close(l.done) }) }
		c = l.conn
	})
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.closed.Do(func() { close(l.done) })
	return nil
}
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
