package env_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/env/env"
	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/mcp"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
)

// fixture runs the full stack the SDK talks to: a fake Substrate control
// plane and router behind a real ate-env-api handler.
type fixture struct {
	router  *fakerouter.Router
	control *fakecontrol.Server
	client  *env.Client
	guest   string // guest workdir
	apiURL  string
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

	service := apiservice.New(directClient, routerAddr, "")
	t.Cleanup(service.Close)

	grpcServer := grpc.NewServer()
	ateenvv1.RegisterEnvironmentServiceServer(grpcServer, service)
	ateenvv1.RegisterProcessServiceServer(grpcServer, service)
	ateenvv1.RegisterFileSystemServiceServer(grpcServer, service)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/v1/envs/{id}/mcp", mcp.NewHandler(directClient))
	mux.Handle("/", grpcServer)

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = &protocols
	srv.Start()
	t.Cleanup(srv.Close)

	client, err := env.NewClient(env.ClientOptions{
		Endpoint: srv.URL,
	})
	if err != nil {
		t.Fatalf("creating SDK client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return &fixture{router: router, control: control, client: client, guest: t.TempDir(), apiURL: srv.URL}
}

// create makes a env whose guest handler serves from a temp dir.
func (f *fixture) create(t *testing.T, id string) *env.Env {
	t.Helper()
	workDir := t.TempDir()
	grpcGuestServer, cleanup, err := guest.NewServer(guest.Config{
		Workspace:        workDir,
		EnableProcess:    true,
		EnableFileSystem: true,
	})
	if err != nil {
		t.Fatalf("creating guest server: %v", err)
	}
	t.Cleanup(cleanup)

	f.router.Register(id, grpcGuestServer)

	sb, err := f.client.Create(t.Context(), &ateenvv1.CreateEnvironmentRequest{
		Id: id,
		Template: &ateenvv1.Template{
			Name:      "default-env",
			Namespace: "envs",
		},
	})
	if err != nil {
		t.Fatalf("creating env %q: %v", id, err)
	}
	return sb
}

func TestCreateStartsEnv(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-1")
	if sb.ID() != "sb-1" {
		t.Errorf("ID = %s, want sb-1", sb.ID())
	}
}

func TestSuspend(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-susp")
	ctx := t.Context()

	if err := sb.Suspend(ctx); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if st := f.control.Status("sb-susp"); st != ateapipb.Actor_STATUS_SUSPENDED {
		t.Errorf("status = %v, want SUSPENDED", st)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-life")
	ctx := t.Context()

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

	res, err := sb.Shell(ctx, "echo 'hi there'")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hi there\n" || res.ExitCode != 0 {
		t.Errorf("cmd result = %+v, want stdout %q", res, "hi there\n")
	}
}

func TestShellStderrAndExitCode(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-stderr")
	ctx := t.Context()

	res, err := sb.Shell(ctx, "echo err-out >&2; exit 42")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stderr) != "err-out" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "err-out")
	}
	if res.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", res.ExitCode)
	}
}

func TestReadFileMissing(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-missing")
	ctx := t.Context()

	_, err := sb.ReadFile(ctx, "nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, env.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLargeFileStreaming(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-large")
	ctx := t.Context()

	// 256 KB file across multiple stream chunks
	largeData := bytes.Repeat([]byte("0123456789abcdef"), 16*1024)
	if err := sb.WriteFile(ctx, "large.dat", bytes.NewReader(largeData), 0o644); err != nil {
		t.Fatal(err)
	}

	rc, err := sb.ReadFile(ctx, "large.dat")
	if err != nil {
		t.Fatal(err)
	}
	readBack, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(readBack) != len(largeData) {
		t.Fatalf("read %d bytes, want %d", len(readBack), len(largeData))
	}
	if !bytes.Equal(readBack, largeData) {
		t.Error("readBack content mismatch")
	}
}
