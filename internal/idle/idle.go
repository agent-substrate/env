// Package idle suspends environments that have gone unused. ate-env-api sees
// every environment operation — gRPC data-plane calls and MCP tool calls — so
// activity is tracked there and a reaper suspends environments whose last
// activity is older than a TTL. Suspension is transparent to clients: the
// atenet router resumes an actor on its next request, and process state
// inside the guest is carried across the checkpoint.
//
// The tracker is in-memory, so TTL accounting assumes a single ate-env-api
// replica. After a restart, running environments are only reaped once they
// are seen again and go idle for a full TTL.
package idle

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// Env identifies an environment by atespace and id.
type Env struct {
	Atespace string
	ID       string
}

type envState struct {
	last time.Time
	pins int
}

// Tracker records per-environment activity.
type Tracker struct {
	mu   sync.Mutex
	envs map[Env]*envState
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{envs: make(map[Env]*envState)}
}

// Touch records activity on env.
func (t *Tracker) Touch(env Env) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state(env).last = time.Now()
}

// Pin marks env busy for the duration of an in-flight operation and returns
// the release func. A pinned environment is never idle, however long the
// operation runs — a shell command or a held output stream must not be
// suspended mid-flight.
func (t *Tracker) Pin(env Env) func() {
	t.mu.Lock()
	s := t.state(env)
	s.last = time.Now()
	s.pins++
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			s := t.state(env)
			s.last = time.Now()
			if s.pins > 0 {
				s.pins--
			}
		})
	}
}

// Forget drops all state for env.
func (t *Tracker) Forget(env Env) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.envs, env)
}

// state returns the entry for env, creating it stamped now.
// Callers must hold t.mu.
func (t *Tracker) state(env Env) *envState {
	s, ok := t.envs[env]
	if !ok {
		s = &envState{last: time.Now()}
		t.envs[env] = s
	}
	return s
}

// idleEnvs returns the environments with no pins whose last activity is at
// least ttl ago.
func (t *Tracker) idleEnvs(ttl time.Duration) []Env {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Env
	for env, s := range t.envs {
		if s.pins == 0 && time.Since(s.last) >= ttl {
			out = append(out, env)
		}
	}
	return out
}

// Client is the slice of the ate client the reaper needs.
type Client interface {
	Get(ctx context.Context, atespace, id string) (*ateapipb.Actor, error)
	Suspend(ctx context.Context, atespace, id string) error
}

// Reaper periodically suspends idle running environments.
type Reaper struct {
	Client  Client
	Tracker *Tracker
	// TTL is how long an environment may be inactive before it is suspended.
	TTL time.Duration
	// Interval is how often to check. Zero derives a value from TTL.
	Interval time.Duration
}

// Run reaps until ctx is canceled.
func (r *Reaper) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = min(r.TTL/2, 30*time.Second)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reap(ctx)
		}
	}
}

func (r *Reaper) reap(ctx context.Context) {
	for _, env := range r.Tracker.idleEnvs(r.TTL) {
		actor, err := r.Client.Get(ctx, env.Atespace, env.ID)
		if errors.Is(err, ate.ErrNotFound) {
			r.Tracker.Forget(env)
			continue
		}
		if err != nil {
			log.Printf("idle reaper: getting %s/%s: %v", env.Atespace, env.ID, err)
			continue
		}
		if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
			// Already suspended (or on its way out); nothing to reap until
			// new activity resumes it.
			r.Tracker.Forget(env)
			continue
		}
		if err := r.Client.Suspend(ctx, env.Atespace, env.ID); err != nil {
			log.Printf("idle reaper: suspending %s/%s: %v", env.Atespace, env.ID, err)
			continue
		}
		r.Tracker.Forget(env)
		log.Printf("idle reaper: suspended %s/%s after %s of inactivity", env.Atespace, env.ID, r.TTL)
	}
}
