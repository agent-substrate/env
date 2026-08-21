// Package service exposes the environment abstraction as a API.
package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/agent-substrate/env/env"
	"github.com/agent-substrate/env/internal/ate"
)

// DefaultTemplate is the ActorTemplate name used when a create request
// does not specify one.
const DefaultTemplate = "default-env"

// DefaultNamespace is the Kubernetes namespace the ActorTemplate is
// looked up in when a create request does not specify one. It matches the
// default namespace of `ate-env manifest`.
const DefaultNamespace = "ate-env"

// Handler serves the environment API backed by client.
func Handler(client *ate.Client) http.Handler {
	s := &server{client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/v1/envs/{id}/{rest...}", s.proxyGuest)
	return mux
}

func (s *server) proxyGuest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rest := r.PathValue("rest")
	if id == "" || rest == "" {
		writeBadRequest(w, "environment id and operation path are required")
		return
	}
	s.client.ProxyGuest(id, "/v1/"+rest, w, r)
}

type server struct {
	client *ate.Client
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeBadRequest(w http.ResponseWriter, format string, args ...any) {
	writeJSON(w, http.StatusBadRequest, env.Error{
		Code:    env.CodeInvalidArgument,
		Message: fmt.Sprintf(format, args...),
	})
}
