package idle

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestTrackerIdleAfterTTL(t *testing.T) {
	tr := NewTracker()
	env := Env{Atespace: "default", ID: "e1"}
	tr.Touch(env)

	if got := tr.idleEnvs(time.Hour); len(got) != 0 {
		t.Fatalf("fresh env reported idle: %v", got)
	}
	time.Sleep(5 * time.Millisecond)
	if got := tr.idleEnvs(time.Millisecond); len(got) != 1 || got[0] != env {
		t.Fatalf("idleEnvs = %v, want [%v]", got, env)
	}
}

func TestTrackerPinBlocksIdle(t *testing.T) {
	tr := NewTracker()
	env := Env{Atespace: "default", ID: "e1"}
	release := tr.Pin(env)

	time.Sleep(5 * time.Millisecond)
	if got := tr.idleEnvs(time.Millisecond); len(got) != 0 {
		t.Fatalf("pinned env reported idle: %v", got)
	}

	release()
	release() // releasing twice must not underflow the pin count

	time.Sleep(5 * time.Millisecond)
	if got := tr.idleEnvs(time.Millisecond); len(got) != 1 {
		t.Fatalf("released env not idle: %v", got)
	}
}

func TestTrackerForget(t *testing.T) {
	tr := NewTracker()
	env := Env{Atespace: "default", ID: "e1"}
	tr.Touch(env)
	tr.Forget(env)
	time.Sleep(5 * time.Millisecond)
	if got := tr.idleEnvs(time.Millisecond); len(got) != 0 {
		t.Fatalf("forgotten env reported idle: %v", got)
	}
}

type fakeClient struct {
	mu        sync.Mutex
	states    map[Env]ateapipb.ActorState
	suspended []Env
}

func (f *fakeClient) Get(ctx context.Context, atespace, id string) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.states[Env{Atespace: atespace, ID: id}]
	if !ok {
		return nil, fmt.Errorf("%w: actor %q", ate.ErrNotFound, id)
	}
	return &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: state}}, nil
}

func (f *fakeClient) Suspend(ctx context.Context, atespace, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = append(f.suspended, Env{Atespace: atespace, ID: id})
	return nil
}

func TestReaperSuspendsIdleRunning(t *testing.T) {
	tr := NewTracker()
	env := Env{Atespace: "default", ID: "e1"}
	tr.Touch(env)
	time.Sleep(5 * time.Millisecond)

	client := &fakeClient{states: map[Env]ateapipb.ActorState{env: ateapipb.ActorState_ACTOR_STATE_RUNNING}}
	r := &Reaper{Client: client, Tracker: tr, TTL: time.Millisecond}
	r.reap(context.Background())

	if len(client.suspended) != 1 || client.suspended[0] != env {
		t.Fatalf("suspended = %v, want [%v]", client.suspended, env)
	}
	// The env is forgotten after suspension: a second pass must not re-suspend.
	r.reap(context.Background())
	if len(client.suspended) != 1 {
		t.Fatalf("re-suspended a forgotten env: %v", client.suspended)
	}
}

func TestReaperSkipsNotRunning(t *testing.T) {
	tr := NewTracker()
	env := Env{Atespace: "default", ID: "e1"}
	tr.Touch(env)
	time.Sleep(5 * time.Millisecond)

	client := &fakeClient{states: map[Env]ateapipb.ActorState{env: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}}
	r := &Reaper{Client: client, Tracker: tr, TTL: time.Millisecond}
	r.reap(context.Background())

	if len(client.suspended) != 0 {
		t.Fatalf("suspended an already-suspended env: %v", client.suspended)
	}
}

func TestReaperForgetsDeletedEnv(t *testing.T) {
	tr := NewTracker()
	env := Env{Atespace: "default", ID: "gone"}
	tr.Touch(env)
	time.Sleep(5 * time.Millisecond)

	client := &fakeClient{states: map[Env]ateapipb.ActorState{}}
	r := &Reaper{Client: client, Tracker: tr, TTL: time.Millisecond}
	r.reap(context.Background())
	if len(client.suspended) != 0 {
		t.Fatalf("suspended a deleted env: %v", client.suspended)
	}
	tr.mu.Lock()
	_, tracked := tr.envs[env]
	tr.mu.Unlock()
	if tracked {
		t.Fatal("deleted env still tracked")
	}
}

func TestUnaryInterceptorTracksMetadataEnv(t *testing.T) {
	tr := NewTracker()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-env-id", "e1", "x-env-atespace", "team"))
	interceptor := UnaryServerInterceptor(tr)

	_, err := interceptor(ctx, struct{}{}, &grpc.UnaryServerInfo{FullMethod: "/ateenv.v1.ProcessService/StartProcess"},
		func(ctx context.Context, req any) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}

	tr.mu.Lock()
	_, tracked := tr.envs[Env{Atespace: "team", ID: "e1"}]
	tr.mu.Unlock()
	if !tracked {
		t.Fatal("env from metadata not tracked")
	}
}

type withID struct{ id string }

func (r withID) GetId() string { return r.id }

func TestUnaryInterceptorForgetsOnSuspend(t *testing.T) {
	tr := NewTracker()
	env := Env{Atespace: ate.DefaultAtespace, ID: "e1"}
	tr.Touch(env)
	interceptor := UnaryServerInterceptor(tr)

	_, err := interceptor(context.Background(), withID{id: "e1"},
		&grpc.UnaryServerInfo{FullMethod: "/ateenv.v1.EnvironmentService/SuspendEnvironment"},
		func(ctx context.Context, req any) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}

	tr.mu.Lock()
	_, tracked := tr.envs[env]
	tr.mu.Unlock()
	if tracked {
		t.Fatal("manually suspended env still tracked")
	}
}

func TestUnaryInterceptorIgnoresGetEnvironment(t *testing.T) {
	tr := NewTracker()
	interceptor := UnaryServerInterceptor(tr)

	_, err := interceptor(context.Background(), withID{id: "e1"},
		&grpc.UnaryServerInfo{FullMethod: "/ateenv.v1.EnvironmentService/GetEnvironment"},
		func(ctx context.Context, req any) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}

	tr.mu.Lock()
	n := len(tr.envs)
	tr.mu.Unlock()
	if n != 0 {
		t.Fatal("status poll tracked as activity")
	}
}
