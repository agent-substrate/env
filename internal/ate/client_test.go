package ate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-substrate/sandbox/internal/ate"
	"github.com/agent-substrate/sandbox/internal/guest"
	"github.com/agent-substrate/sandbox/internal/guest/guestsys"
	"github.com/agent-substrate/sandbox/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/sandbox/internal/internaltest/fakerouter"
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

// create makes a sandbox whose guest handler serves from a temp dir.
func (f *fixture) create(t *testing.T, id string, opts ...ate.CreateOption) *ate.Sandbox {
	t.Helper()
	fsSys, _ := guestsys.New(f.guest)
	h, err := (&guest.Server{FS: fsSys}).Handler()
	if err != nil {
		t.Fatalf("creating guest handler: %v", err)
	}
	f.router.Register(id, h)
	opts = append([]ate.CreateOption{ate.WithTemplate("default"), ate.WithNamespace("sandboxes")}, opts...)
	sb, err := f.client.Create(t.Context(), id, opts...)
	if err != nil {
		t.Fatalf("creating sandbox %q: %v", id, err)
	}
	return sb
}

func TestCreateStartsSandbox(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-1")

	info, err := sb.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != ate.StatusRunning {
		t.Errorf("status = %s, want running", info.Status)
	}
	if info.Namespace != "sandboxes" || info.Template != "default" {
		t.Errorf("template = %s/%s, want sandboxes/default", info.Namespace, info.Template)
	}
}

func TestSuspendResumeCycle(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-cycle")
	ctx := t.Context()

	if err := sb.Suspend(ctx); err != nil {
		t.Fatal(err)
	}
	if info, _ := sb.Info(ctx); info.Status != ate.StatusSuspended {
		t.Fatalf("status after suspend = %s, want suspended", info.Status)
	}
	if err := sb.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if info, _ := sb.Info(ctx); info.Status != ate.StatusRunning {
		t.Fatalf("status after resume = %s, want running", info.Status)
	}
	if err := sb.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if info, _ := sb.Info(ctx); info.Status != ate.StatusPaused {
		t.Fatalf("status after pause = %s, want paused", info.Status)
	}
}

func TestDeleteSuspendsRunningSandboxFirst(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-del")
	ctx := t.Context()

	// The fake control plane rejects deleting non-suspended actors, so
	// this passing proves Delete suspends first.
	if err := sb.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Info(ctx); !errors.Is(err, ate.ErrNotFound) {
		t.Errorf("Info after delete = %v, want ErrNotFound", err)
	}
}

func TestOpenMissingSandbox(t *testing.T) {
	f := newFixture(t)
	if _, err := f.client.Open(t.Context(), "does-not-exist"); !errors.Is(err, ate.ErrNotFound) {
		t.Errorf("Open = %v, want ErrNotFound", err)
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

	if _, err := client.Create(context.Background(), "sb-x"); err == nil {
		t.Fatal("Create without template succeeded, want error")
	}
}
