package sandbox_test

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/sandbox/internal/ate"
	"github.com/agent-substrate/sandbox/internal/guest"
	"github.com/agent-substrate/sandbox/internal/guest/guestsys"
	"github.com/agent-substrate/sandbox/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/sandbox/internal/internaltest/fakerouter"
	"github.com/agent-substrate/sandbox/internal/service"
	"github.com/agent-substrate/sandbox/sandbox"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// fixture runs the full stack the SDK talks to: a fake Substrate control
// plane and router behind a real sbx-api handler.
type fixture struct {
	router *fakerouter.Router
	client *sandbox.Client
	guest  string // guest workdir
	apiURL string
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

	directClient, err := ate.New(ate.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatalf("creating direct client: %v", err)
	}
	t.Cleanup(func() { directClient.Close() })

	srv := httptest.NewServer(service.Handler(directClient))
	t.Cleanup(srv.Close)

	client, err := sandbox.NewClient(sandbox.ClientOptions{
		Endpoint: srv.URL,
	})
	if err != nil {
		t.Fatalf("creating SDK client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return &fixture{router: router, client: client, guest: t.TempDir(), apiURL: srv.URL}
}

// create makes a sandbox whose guest handler serves from a temp dir.
func (f *fixture) create(t *testing.T, id string, opts ...sandbox.CreateOption) *sandbox.Sandbox {
	t.Helper()
	fsSys, _ := guestsys.New(f.guest)
	h, err := (&guest.Server{}).Handler(fsSys)
	if err != nil {
		t.Fatalf("creating guest handler: %v", err)
	}
	f.router.Register(id, h)
	opts = append([]sandbox.CreateOption{sandbox.WithTemplate("default"), sandbox.WithNamespace("sandboxes")}, opts...)
	sb, err := f.client.Create(t.Context(), id, opts...)
	if err != nil {
		t.Fatalf("creating sandbox %q: %v", id, err)
	}
	return sb
}

func TestCreateStartsSandbox(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-1")
	if sb.ID() != "sb-1" {
		t.Errorf("ID = %s, want sb-1", sb.ID())
	}
}

func TestLifecycle(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-life")
	ctx := t.Context()

	if err := sb.Suspend(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sb.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sb.Suspend(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sb.Delete(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCmdAndFilesystem(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-fs")
	ctx := t.Context()

	if err := sb.WriteFile(ctx, "project/hello.txt", strings.NewReader("hi there"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, err := sb.ReadFile(ctx, "project/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hi there" {
		t.Errorf("read back %q, want %q", data, "hi there")
	}

	res, err := sb.Cmd(ctx, "cat project/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hi there" || res.ExitCode != 0 {
		t.Errorf("cmd result = %+v, want stdout %q", res, "hi there")
	}

	entries, err := sb.ListDir(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "hello.txt" {
		t.Errorf("listing = %+v, want [hello.txt]", entries)
	}

	entry, err := sb.Stat(ctx, "project/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if entry.IsDir || entry.Size != int64(len("hi there")) {
		t.Errorf("stat = %+v, want regular file of %d bytes", entry, len("hi there"))
	}

	if err := sb.Mkdir(ctx, "project/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sb.Remove(ctx, "project/hello.txt"); err != nil {
		t.Fatal(err)
	}
	if err := sb.Remove(ctx, "project"); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Stat(ctx, "project"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("stat after remove = %v, want ErrNotFound", err)
	}
}
