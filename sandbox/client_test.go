package sandbox_test

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/agent-substrate/sandbox/internal/ate"
	"github.com/agent-substrate/sandbox/internal/guest"
	"github.com/agent-substrate/sandbox/internal/guest/guestsys"
	"github.com/agent-substrate/sandbox/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/sandbox/internal/internaltest/fakerouter"
	"github.com/agent-substrate/sandbox/internal/service"
	"github.com/agent-substrate/sandbox/sandbox"
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
		Endpoint:  srv.URL,
		Template:  "default",
		Namespace: "sandboxes",
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
	h, err := (&guest.Server{FS: fsSys}).Handler()
	if err != nil {
		t.Fatalf("creating guest handler: %v", err)
	}
	f.router.Register(id, h)
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
	if info.Status != sandbox.StatusRunning {
		t.Errorf("status = %s, want running", info.Status)
	}
	if info.Namespace != "sandboxes" || info.Template != "default" {
		t.Errorf("template = %s/%s, want sandboxes/default", info.Namespace, info.Template)
	}
}

func TestLifecycle(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-life")
	ctx := t.Context()

	if err := sb.Suspend(ctx); err != nil {
		t.Fatal(err)
	}
	if info, _ := sb.Info(ctx); info.Status != sandbox.StatusSuspended {
		t.Errorf("after suspend = %s, want suspended", info.Status)
	}
	if err := sb.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if info, _ := sb.Info(ctx); info.Status != sandbox.StatusRunning {
		t.Errorf("after resume = %s, want running", info.Status)
	}
	if err := sb.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Info(ctx); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("info after delete = %v, want ErrNotFound", err)
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

func TestWorkdirOption(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-workdir")

	client, err := sandbox.NewClient(sandbox.ClientOptions{
		Endpoint: f.apiURL,
		Workdir:  "workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx := t.Context()
	sb = client.Sandbox(sb.ID())

	if err := sb.WriteFile(ctx, "test.txt", strings.NewReader("hello workdir"), 0o644); err != nil {
		t.Fatal(err)
	}

	rc, err := sb.ReadFile(ctx, "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello workdir" {
		t.Errorf("content = %q, want %q", string(content), "hello workdir")
	}

	entry, err := sb.Stat(ctx, "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "test.txt" {
		t.Errorf("entry.Name = %q, want %q", entry.Name, "test.txt")
	}
}



func TestOpen(t *testing.T) {
	f := newFixture(t)
	f.create(t, "sb-a")
	ctx := t.Context()

	if _, err := f.client.Open(ctx, "sb-a"); err != nil {
		t.Errorf("open sb-a: %v", err)
	}
	if _, err := f.client.Open(ctx, "absent"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("open absent = %v, want ErrNotFound", err)
	}
}
