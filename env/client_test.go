package env_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/env/env"
	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/guest"
	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/service"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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
		return control.State(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
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

	grpcServer := grpc.NewServer()
	ateenvv1.RegisterEnvironmentServiceServer(grpcServer, apiservice.New(directClient))
	httpHandler := service.Handler(directClient)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		httpHandler.ServeHTTP(w, r)
	})
	h2cHandler := h2c.NewHandler(handler, &http2.Server{})

	srv := httptest.NewServer(h2cHandler)
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
	sys := guestsys.New()
	h, err := (&guest.Server{}).Handler(sys)
	if err != nil {
		t.Fatalf("creating guest handler: %v", err)
	}
	f.router.Register(id, h)
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

	if st := f.control.State("sb-susp"); st != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
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

	res, err := sb.Shell(ctx, "cat project/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hi there" || res.ExitCode != 0 {
		t.Errorf("cmd result = %+v, want stdout %q", res, "hi there")
	}
}
