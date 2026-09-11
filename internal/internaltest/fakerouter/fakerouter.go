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
	id, ok := actorID(req)
	if !ok {
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

func actorID(req *http.Request) (string, bool) {
	if targetActor := req.Header.Get(ate.TargetActorHeader); targetActor != "" {
		_, actor, ok := strings.Cut(targetActor, "/")
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
