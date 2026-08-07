// Package service exposes the environment abstraction as a API.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/guest"
)

// DefaultTemplate is the ActorTemplate name used when a create request
// does not specify one.
const DefaultTemplate = "env"

// DefaultNamespace is the Kubernetes namespace the ActorTemplate is
// looked up in when a create request does not specify one. It matches the
// default namespace of `ate-env deploy`.
const DefaultNamespace = "ate-env"

// CreateEnvRequest is the body of POST /v1/envs.
type CreateEnvRequest struct {
	// ID is the environment identifier (a DNS-1123 label). Required.
	ID string `json:"id"`

	// Template is the name of the ActorTemplate the environment is created
	// from. Defaults to the service's default template.
	Template string `json:"template,omitempty"`

	// Namespace is the Kubernetes namespace the ActorTemplate lives in.
	// Defaults to "ate-env", the default namespace of
	// `ate-env deploy`.
	Namespace string `json:"namespace,omitempty"`
}

// FSRequest is the body of the filesystem endpoints
// (POST /v1/envs/{id}/{file,dir,stat}).
type FSRequest struct {
	// Path of the file or directory inside the environment. Relative paths
	// resolve against the guest's workdir. Required.
	Path string `json:"path"`

	// Mode is the octal file mode for write and mkdir, e.g. "644".
	// Defaults to "644" for files and "755" for directories.
	Mode string `json:"mode,omitempty"`

	// Content is the file content for write. It is base64-encoded in
	// JSON.
	Content []byte `json:"content,omitempty"`
}

// Handler serves the environment API backed by client.
func Handler(client *ate.Client) http.Handler {
	s := &server{client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /v1/envs", s.create)
	mux.HandleFunc("DELETE /v1/envs/{id}", s.delete)
	mux.HandleFunc("POST /v1/envs/{id}/suspend", s.lifecycle((*ate.ActorClient).Suspend))
	mux.HandleFunc("POST /v1/envs/{id}/resume", s.lifecycle((*ate.ActorClient).Resume))
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
	code := guest.CodeInternal
	if errors.Is(err, ate.ErrNotFound) {
		status = http.StatusNotFound
		code = guest.CodeNotFound
	}
	writeJSON(w, status, guest.Error{Code: code, Message: err.Error()})
}

func writeBadRequest(w http.ResponseWriter, format string, args ...any) {
	writeJSON(w, http.StatusBadRequest, guest.Error{
		Code:    guest.CodeInvalidArgument,
		Message: fmt.Sprintf(format, args...),
	})
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var req CreateEnvRequest
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
	opts := []ate.CreateOption{
		ate.WithTemplate(req.Template),
		ate.WithNamespace(req.Namespace),
	}
	_, err := s.client.Create(r.Context(), req.ID, opts...)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *server) delete(w http.ResponseWriter, r *http.Request) {
	if err := s.client.Actor(r.PathValue("id")).Delete(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) lifecycle(op func(*ate.ActorClient, context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sb := s.client.Actor(r.PathValue("id"))
		if err := op(sb, r.Context()); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
