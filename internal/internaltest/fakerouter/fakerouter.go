// Package fakerouter implements a test double for the atenet router: it
// resolves the target sandbox from the request's Host header and forwards
// to that sandbox's guest handler, returning 503 when the sandbox is not
// running — mirroring a router with auto-resume disabled.
package fakerouter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/agent-substrate/sandbox/internal/direct"
)

// Router is a fake atenet router.
type Router struct {
	// Running reports whether the actor with the given ID is currently
	// running. Non-running actors get a 503.
	Running func(id string) bool

	mu     sync.Mutex
	guests map[string]http.Handler
}

// New returns a Router that considers every actor running unless a Running
// callback is set.
func New() *Router {
	return &Router{guests: make(map[string]http.Handler)}
}

// Register installs the guest handler serving a sandbox ID.
func (r *Router) Register(id string, h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guests[id] = h
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id, _, ok := strings.Cut(req.Host, ".")
	if !ok || !strings.HasSuffix(req.Host, "."+direct.DefaultHostSuffix) {
		http.Error(w, "unroutable host "+req.Host, http.StatusNotFound)
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

// Serve starts the router on a random localhost port and returns its
// address and a shutdown function.
func (r *Router) Serve() (addr string, stop func()) {
	srv := httptest.NewServer(r)
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}
