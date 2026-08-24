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
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
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
	mux.HandleFunc("GET /v1/envs", s.list)
	// More specific than the guest proxy below, so it wins the route match
	// rather than being forwarded into the environment.
	mux.HandleFunc("GET /v1/envs/{id}", s.get)
	mux.HandleFunc("/v1/envs/{id}/{rest...}", s.proxyGuest)
	return mux
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	actors, err := s.client.List(r.Context(), r.URL.Query().Get("atespace"))
	if err != nil {
		writeErr(w, err)
		return
	}
	infos := make([]env.EnvInfo, len(actors))
	for i, a := range actors {
		infos[i] = actorToInfo(a)
	}
	writeJSON(w, http.StatusOK, infos)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	actor, err := s.client.Get(r.Context(), r.URL.Query().Get("atespace"), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, actorToInfo(actor))
}

// actorToInfo converts a Substrate actor to the EnvInfo the API returns.
func actorToInfo(a *ateapipb.Actor) env.EnvInfo {
	return env.EnvInfo{
		ID:                a.GetMetadata().GetName(),
		Atespace:          a.GetMetadata().GetAtespace(),
		Template:          a.GetActorTemplateName(),
		TemplateNamespace: a.GetActorTemplateNamespace(),
		Status:            stateString(a.GetStatus().GetState()),
	}
}

// stateString maps an actor state to the status string the API returns.
// These strings are client API; changes break existing clients.
func stateString(st ateapipb.ActorState) string {
	switch st {
	case ateapipb.ActorState_ACTOR_STATE_RESUMING:
		return "resuming"
	case ateapipb.ActorState_ACTOR_STATE_RUNNING:
		return "running"
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
		return "suspending"
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		return "suspended"
	case ateapipb.ActorState_ACTOR_STATE_PAUSING:
		return "pausing"
	case ateapipb.ActorState_ACTOR_STATE_PAUSED:
		return "paused"
	case ateapipb.ActorState_ACTOR_STATE_CRASHED:
		return "crashed"
	case ateapipb.ActorState_ACTOR_STATE_DELETING:
		return "deleting"
	default:
		return "unknown"
	}
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

func writeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ate.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, env.Error{Code: env.CodeNotFound, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, env.Error{Code: env.CodeInternal, Message: err.Error()})
}
