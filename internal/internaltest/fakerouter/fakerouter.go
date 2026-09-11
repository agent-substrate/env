// Package fakerouter implements a test double for the atenet router: it
// resolves the target environment from the request's Host header and forwards
// to that environment's guest handler, returning 503 when the environment is not
// running.
package fakerouter

import (
	"net/http"
	"net/http/httptest"
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
}

// New returns a Router that considers every actor running unless a Running
// callback is set.
func New() *Router {
	return &Router{guests: make(map[string]http.Handler)}
}

// Register installs the guest handler serving a environment ID.
func (r *Router) Register(id string, h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guests[id] = h
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var id string
	if targetActor := req.Header.Get(ate.TargetActorHeader); targetActor != "" {
		_, actor, ok := strings.Cut(targetActor, "/")
		if !ok || actor == "" {
			http.Error(w, "invalid actor reference", http.StatusNotFound)
			return
		}
		id = actor
	} else if strings.Contains(req.Host, ".") {
		var ok bool
		id, _, ok = strings.Cut(req.Host, ".")
		if !ok || !strings.HasSuffix(req.Host, "."+ate.DefaultHostSuffix) {
			http.Error(w, "invalid actor reference", http.StatusNotFound)
			return
		}
	} else {
		http.Error(w, "invalid actor reference", http.StatusNotFound)
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
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := httptest.NewUnstartedServer(r)
	srv.Config.Protocols = &protocols
	srv.Start()
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}
