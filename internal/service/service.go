// Package service exposes the environment abstraction as a API.
package service

import (
	"encoding/json"
	"errors"
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
// default namespace of `ate-env deploy`.
const DefaultNamespace = "ate-env"

// Handler serves the environment API backed by client.
func Handler(client *ate.Client) http.Handler {
	s := &server{client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /v1/envs", s.create)
	mux.HandleFunc("DELETE /v1/envs/{id}", s.delete)
	// More specific than the guest proxy below, so it wins the route match
	// rather than being forwarded into the environment.
	mux.HandleFunc("POST /v1/envs/{id}/fork", s.fork)
	mux.HandleFunc("POST /v1/envs/{id}/suspend", s.suspend)
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

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := env.CodeInternal
	switch {
	case errors.Is(err, ate.ErrNotFound):
		status = http.StatusNotFound
		code = env.CodeNotFound
	case errors.Is(err, ate.ErrPrecondition):
		status = http.StatusConflict
		code = env.CodeFailedPrecondition
	}
	writeJSON(w, status, env.Error{Code: code, Message: err.Error()})
}

func writeBadRequest(w http.ResponseWriter, format string, args ...any) {
	writeJSON(w, http.StatusBadRequest, env.Error{
		Code:    env.CodeInvalidArgument,
		Message: fmt.Sprintf(format, args...),
	})
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var req env.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "invalid request body: %v", err)
		return
	}
	if req.ID == "" {
		writeBadRequest(w, "id is required")
		return
	}
	if req.Template == "" {
		req.Template = DefaultTemplate
	}
	if req.Namespace == "" {
		req.Namespace = DefaultNamespace
	}
	if err := s.client.Create(r.Context(), req); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *server) fork(w http.ResponseWriter, r *http.Request) {
	var req env.ForkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "invalid request body: %v", err)
		return
	}
	if req.DestID == "" {
		writeBadRequest(w, "dest_id is required")
		return
	}
	if err := s.client.Fork(r.Context(), r.PathValue("id"), req.DestID); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *server) suspend(w http.ResponseWriter, r *http.Request) {
	if err := s.client.Suspend(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) delete(w http.ResponseWriter, r *http.Request) {
	if err := s.client.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
