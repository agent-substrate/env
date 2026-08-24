package ate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/guest"
	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

type fixture struct {
	control *fakecontrol.Server
	router  *fakerouter.Router
	client  *ate.Client
	guest   string // guest workdir
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Chdir(t.TempDir())

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatalf("starting fake control plane: %v", err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.State(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
	}
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	client, err := ate.New(ate.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return &fixture{control: control, router: router, client: client, guest: t.TempDir()}
}

// create makes a env whose guest handler serves from a temp dir.
func (f *fixture) create(t *testing.T, id string) {
	t.Helper()
	sys := guestsys.New()
	h, err := (&guest.Server{}).Handler(sys)
	if err != nil {
		t.Fatalf("creating guest handler: %v", err)
	}
	f.router.Register(id, h)
	req := ate.CreateOptions{ID: id, Template: "default-env", Namespace: "envs"}
	if err := f.client.Create(t.Context(), req); err != nil {
		t.Fatalf("creating actor %q: %v", id, err)
	}
}

func TestCreateStartsEnv(t *testing.T) {
	f := newFixture(t)
	f.create(t, "sb-1")
}

func TestEnsureAtespace(t *testing.T) {
	f := newFixture(t)
	if err := f.client.EnsureAtespace(t.Context(), "custom-space"); err != nil {
		t.Fatalf("EnsureAtespace: %v", err)
	}
	// Ensuring again should be a no-op (AlreadyExists ignored)
	if err := f.client.EnsureAtespace(t.Context(), "custom-space"); err != nil {
		t.Fatalf("EnsureAtespace idempotent call: %v", err)
	}
}

func TestSuspend(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "sb-susp")

	if err := f.client.Suspend(ctx, "", "sb-susp"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if st := f.control.State("sb-susp"); st != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("status = %v, want SUSPENDED", st)
	}
}

func TestSuspendMissing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	if err := f.client.Suspend(ctx, "", "nonexistent"); !errors.Is(err, ate.ErrNotFound) {
		t.Fatalf("Suspend nonexistent: err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "sb-life")

	if err := f.client.Delete(ctx, "", "sb-life"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestCreateRequiresTemplate(t *testing.T) {
	control := fakecontrol.New()
	addr, stop, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	client, err := ate.New(ate.Options{ControlAddr: addr, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	if err := client.Create(context.Background(), ate.CreateOptions{ID: "sb-x"}); err == nil {
		t.Fatal("Create without template succeeded, want error")
	}
}
