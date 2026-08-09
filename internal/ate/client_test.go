package ate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-substrate/env/env"
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

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatalf("starting fake control plane: %v", err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.Status(id) == ateapipb.Actor_STATUS_RUNNING
	}
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	guestDir := t.TempDir()
	t.Chdir(guestDir)

	client, err := ate.New(ate.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	f := &fixture{control: control, router: router, client: client, guest: guestDir}
	return f
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
	req := env.CreateRequest{ID: id, Template: "default-env", Namespace: "envs"}
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

func TestDelete(t *testing.T) {
	f := newFixture(t)
	f.create(t, "sb-del")
	ctx := t.Context()

	if err := f.client.Delete(ctx, "sb-del"); err != nil {
		t.Fatal(err)
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

	if err := client.Create(context.Background(), env.CreateRequest{ID: "sb-x"}); err == nil {
		t.Fatal("Create without template succeeded, want error")
	}
}

func TestFork(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "sb-src")

	// A running actor that is not suspended cannot be forked.
	if err := f.client.Fork(ctx, "sb-src", "sb-fork"); !errors.Is(err, ate.ErrPrecondition) {
		t.Fatalf("fork of non-suspended source: err = %v, want ErrPrecondition", err)
	}

	snapshot := f.control.Suspend("sb-src")
	if snapshot == "" {
		t.Fatal("suspend produced no snapshot")
	}

	if err := f.client.Fork(ctx, "sb-src", "sb-fork"); err != nil {
		t.Fatalf("fork: %v", err)
	}
	if got := f.control.SnapshotOf("sb-fork"); got != snapshot {
		t.Errorf("fork created from snapshot %q, want %q", got, snapshot)
	}
	if got := f.control.Status("sb-src"); got != ateapipb.Actor_STATUS_SUSPENDED {
		t.Errorf("source status after fork = %v, want SUSPENDED", got)
	}
}

func TestForkRejectsResumingSource(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "sb-resuming")
	f.control.Suspend("sb-resuming")
	f.control.SetStatus("sb-resuming", ateapipb.Actor_STATUS_RESUMING)

	// It has a snapshot, but its state is still in flight.
	if err := f.client.Fork(ctx, "sb-resuming", "sb-fork"); !errors.Is(err, ate.ErrPrecondition) {
		t.Fatalf("fork of a resuming source: err = %v, want ErrPrecondition", err)
	}
}

func TestForkMissingSource(t *testing.T) {
	f := newFixture(t)
	if err := f.client.Fork(t.Context(), "sb-nope", "sb-fork"); !errors.Is(err, ate.ErrNotFound) {
		t.Fatalf("fork of a missing source: err = %v, want ErrNotFound", err)
	}
}
