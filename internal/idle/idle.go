// Package idle suspends environments nobody is using. A Tracker
// records each environment's last data-plane activity and pins
// environments with in-flight calls — a held long-poll or a slow
// upload never reaps. A Reaper periodically suspends the running
// environments the tracker reports idle.
//
// State is in memory by design: the api service runs single-replica,
// and after a restart every environment simply gets a fresh TTL of
// grace before it can be reaped. Suspension is transparent to clients
// because resume lives in the atenet router, not here — which is also
// why a call racing the reaper costs at most a retried request, never
// correctness.
package idle

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// Tracker records per-environment activity. The zero value is ready to
// use.
type Tracker struct {
	mu   sync.Mutex
	envs map[string]*entry
}

type entry struct {
	last    time.Time
	streams int
}

func (t *Tracker) entryLocked(id string) *entry {
	if t.envs == nil {
		t.envs = make(map[string]*entry)
	}
	e := t.envs[id]
	if e == nil {
		e = &entry{}
		t.envs[id] = e
	}
	return e
}

// Touch records activity on env id now.
func (t *Tracker) Touch(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entryLocked(id).last = time.Now()
}

// StreamStart pins env id while a call is in flight. Every StreamStart
// must be paired with a StreamEnd.
func (t *Tracker) StreamStart(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entryLocked(id)
	e.streams++
	e.last = time.Now()
}

// StreamEnd unpins env id and records the activity. It is a no-op for
// an unknown env: when Forget raced an in-flight call, recreating the
// entry here would drive the count negative and pin the environment
// forever.
func (t *Tracker) StreamEnd(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.envs[id]
	if e == nil {
		return
	}
	if e.streams > 0 {
		e.streams--
	}
	e.last = time.Now()
}

// Forget drops env id's state, e.g. when the environment is deleted.
func (t *Tracker) Forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.envs, id)
}

// prune drops state for every env not in exists, so ids that were
// deleted (or never existed — the data plane cannot check) do not
// accumulate forever.
func (t *Tracker) prune(exists map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.envs {
		if !exists[id] {
			delete(t.envs, id)
		}
	}
}

// Idle reports whether env id has had no activity for at least ttl and
// has no in-flight calls. An environment the tracker has never seen is
// not idle — asking starts its clock, which is what grants every
// environment one TTL of grace after the service restarts.
func (t *Tracker) Idle(id string, ttl time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.envs[id]
	if e == nil {
		t.entryLocked(id).last = time.Now()
		return false
	}
	return e.streams == 0 && time.Since(e.last) >= ttl
}

// Client is the part of the environment client the reaper needs.
type Client interface {
	List(ctx context.Context, atespace string) ([]*ateapipb.Actor, error)
	Suspend(ctx context.Context, atespace, id string) error
}

// Reaper suspends running environments that Tracker reports idle.
type Reaper struct {
	Client  Client
	Tracker *Tracker

	// TTL is how long an environment must be inactive before it is
	// suspended. Required.
	TTL time.Duration

	// Interval is how often the reaper scans. Defaults to TTL/2.
	Interval time.Duration

	// Atespace bounds the scan. The data plane addresses environments
	// by bare id, so the tracker's records are only meaningful within
	// one atespace.
	Atespace string
}

// Run scans until ctx is done.
func (r *Reaper) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = r.TTL / 2
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

// reap suspends every running environment that is idle and prunes
// tracker state for environments that no longer exist.
func (r *Reaper) reap(ctx context.Context) {
	actors, err := r.Client.List(ctx, r.Atespace)
	if err != nil {
		log.Printf("idle: listing environments: %v", err)
		return
	}
	exists := make(map[string]bool, len(actors))
	for _, actor := range actors {
		exists[actor.GetMetadata().GetName()] = true
	}
	r.Tracker.prune(exists)

	for _, actor := range actors {
		if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
			continue
		}
		id := actor.GetMetadata().GetName()
		if !r.Tracker.Idle(id, r.TTL) {
			continue
		}
		if err := r.Client.Suspend(ctx, actor.GetMetadata().GetAtespace(), id); err != nil {
			log.Printf("idle: suspending %q: %v", id, err)
			continue
		}
		log.Printf("idle: suspended %q after %s of inactivity", id, r.TTL)
		r.Tracker.Forget(id)
	}
}
