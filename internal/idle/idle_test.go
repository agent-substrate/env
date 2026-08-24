package idle_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/idle"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func TestTrackerTouchAndIdle(t *testing.T) {
	tr := &idle.Tracker{}

	// Never-seen environments are not idle; asking starts their clock.
	if tr.Idle("fresh", 0) {
		t.Error("never-seen env reported idle, want one TTL of grace")
	}
	if !tr.Idle("fresh", 0) {
		t.Error("env not idle with a zero TTL after its clock started")
	}

	tr.Touch("busy")
	if tr.Idle("busy", time.Hour) {
		t.Error("just-touched env reported idle")
	}
	if !tr.Idle("busy", 0) {
		t.Error("touched env not idle with a zero TTL")
	}
}

func TestTrackerStreamsPin(t *testing.T) {
	tr := &idle.Tracker{}

	tr.StreamStart("held")
	if tr.Idle("held", 0) {
		t.Error("env with an in-flight stream reported idle")
	}
	tr.StreamStart("held")
	tr.StreamEnd("held")
	if tr.Idle("held", 0) {
		t.Error("env with one of two streams still open reported idle")
	}
	tr.StreamEnd("held")
	if !tr.Idle("held", 0) {
		t.Error("env not idle after its last stream ended")
	}
	if tr.Idle("held", time.Hour) {
		t.Error("stream end did not count as activity")
	}
}

func TestTrackerForget(t *testing.T) {
	tr := &idle.Tracker{}
	tr.Touch("gone")
	tr.Forget("gone")
	if tr.Idle("gone", 0) {
		t.Error("forgotten env reported idle, want a fresh grace period")
	}
}

// TestTrackerForgetDuringStream pins the Forget/StreamEnd race: a
// suspend or delete that races an in-flight call must not corrupt the
// stream count, or the environment could never be reaped again.
func TestTrackerForgetDuringStream(t *testing.T) {
	tr := &idle.Tracker{}
	tr.StreamStart("racy")
	tr.Forget("racy")
	tr.StreamEnd("racy")

	// First Idle starts the fresh grace period; after it the env must
	// be reapable — a leaked negative stream count would pin it forever.
	tr.Idle("racy", 0)
	if !tr.Idle("racy", 0) {
		t.Error("env not idle after Forget raced StreamEnd; stream count corrupted")
	}
}

// fakeClient implements idle.Client in memory.
type fakeClient struct {
	mu        sync.Mutex
	actors    []*ateapipb.Actor
	suspended []string
}

func runningActor(atespace, name string) *ateapipb.Actor {
	return &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: name},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
}

func (f *fakeClient) List(ctx context.Context, atespace string) ([]*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*ateapipb.Actor(nil), f.actors...), nil
}

func (f *fakeClient) Suspend(ctx context.Context, atespace, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = append(f.suspended, id)
	for _, a := range f.actors {
		if a.GetMetadata().GetName() == id {
			a.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}
		}
	}
	return nil
}

func (f *fakeClient) suspendedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.suspended...)
}

func TestReaperSuspendsOnlyIdleEnvs(t *testing.T) {
	client := &fakeClient{actors: []*ateapipb.Actor{
		runningActor("default", "idle-env"),
		runningActor("default", "active-env"),
		runningActor("default", "pinned-env"),
		{
			Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: "already-suspended"},
			Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
		},
	}}
	tracker := &idle.Tracker{}
	reaper := &idle.Reaper{
		Client:   client,
		Tracker:  tracker,
		TTL:      50 * time.Millisecond,
		Interval: 10 * time.Millisecond,
		Atespace: "default",
	}

	tracker.Touch("idle-env")
	tracker.StreamStart("pinned-env")
	// active-env stays fresh via periodic touches below; unknown-env is
	// deliberately never mentioned: the reaper discovers it and must
	// still grant it a full TTL before reaping.
	client.mu.Lock()
	client.actors = append(client.actors, runningActor("default", "unknown-env"))
	client.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reaper.Run(ctx)

	deadline := time.After(10 * time.Second)
	for {
		tracker.Touch("active-env")
		ids := client.suspendedIDs()
		if len(ids) >= 2 {
			got := map[string]bool{}
			for _, id := range ids {
				got[id] = true
			}
			if !got["idle-env"] || !got["unknown-env"] || len(got) != 2 {
				t.Fatalf("suspended %v, want exactly idle-env and unknown-env", ids)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("reaper suspended %v, want idle-env and unknown-env", ids)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
