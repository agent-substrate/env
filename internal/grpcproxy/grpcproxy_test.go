package grpcproxy_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/grpcmux"
	"github.com/agent-substrate/env/internal/grpcproxy"
	"github.com/agent-substrate/env/internal/guest/guestrpc"
	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/guest/proc"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	pb "github.com/agent-substrate/env/proto/ateenv/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fixture runs the full data plane the proxy sits in: guests behind a
// fake router, an ate.Client on a fake control plane, and an api-side
// gRPC server proxying unknown methods to guests.
type fixture struct {
	control *fakecontrol.Server
	router  *fakerouter.Router
	client  *ate.Client
	conn    *grpc.ClientConn
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Chdir(t.TempDir())

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	// The api-side server, wired like cmd/ate-env-api: typed
	// EnvironmentService plus the opaque proxy for everything else.
	grpcServer := grpc.NewServer(
		grpc.ForceServerCodec(grpcproxy.Codec()),
		grpc.UnknownServiceHandler(grpcproxy.StreamHandler(
			func(ctx context.Context, envID string) (*grpc.ClientConn, error) {
				return client.GuestConn(envID)
			},
		)),
	)
	ateenvv1.RegisterEnvironmentServiceServer(grpcServer, apiservice.New(client))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpcmux.Server(grpcmux.Handler(grpcServer, http.NotFoundHandler()))
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Close() })

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	return &fixture{control: control, router: router, client: client, conn: conn}
}

// createEnv registers a guest serving the full guest gRPC stack and
// creates the backing actor.
func (f *fixture) createEnv(t *testing.T, id string) {
	t.Helper()
	guestGrpc := grpc.NewServer()
	guestrpc.Register(guestGrpc, guestsys.New(), proc.NewTable())
	f.router.Register(id, grpcmux.Handler(guestGrpc, http.NotFoundHandler()))
	t.Cleanup(guestGrpc.Stop)

	if err := f.client.Create(t.Context(), ate.CreateOptions{
		ID:        id,
		Template:  "default-env",
		Namespace: "envs",
	}); err != nil {
		t.Fatalf("creating env %q: %v", id, err)
	}
}

// envCtx returns a context addressing calls to env id.
func envCtx(ctx context.Context, id string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, grpcproxy.MetadataKey, id)
}

func TestProxyUnary(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-1")

	res, err := pb.NewProcessClient(f.conn).Exec(envCtx(t.Context(), "env-1"), &pb.ExecRequest{
		Command: "printf 'through the proxy'",
	})
	if err != nil {
		t.Fatalf("Exec through proxy: %v", err)
	}
	if got := string(res.GetStdout()); got != "through the proxy" || res.GetExitCode() != 0 {
		t.Fatalf("Exec = %q (exit %d), want %q", got, res.GetExitCode(), "through the proxy")
	}
}

func TestProxyStreaming(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-stream")
	ctx := envCtx(t.Context(), "env-stream")
	fs := pb.NewFileSystemClient(f.conn)

	// Client-streaming write, in chunks larger than the guest's read
	// chunk so the server-streaming read comes back in several pieces.
	content := bytes.Repeat([]byte("0123456789abcdef"), 16<<10) // 256 KiB
	w, err := fs.WriteFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(&pb.WriteFileRequest{Path: "data.bin", Data: content[:100<<10]}); err != nil {
		t.Fatal(err)
	}
	if err := w.Send(&pb.WriteFileRequest{Data: content[100<<10:]}); err != nil {
		t.Fatal(err)
	}
	wres, err := w.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteFile through proxy: %v", err)
	}
	if wres.GetBytesWritten() != int64(len(content)) {
		t.Fatalf("BytesWritten = %d, want %d", wres.GetBytesWritten(), len(content))
	}

	r, err := fs.ReadFile(ctx, &pb.ReadFileRequest{Path: "data.bin"})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	chunks := 0
	for {
		resp, err := r.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadFile through proxy: %v", err)
		}
		chunks++
		got = append(got, resp.GetData()...)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("read back %d bytes, want %d", len(got), len(content))
	}
	if chunks < 2 {
		t.Fatalf("read back in %d chunk(s), want a real stream", chunks)
	}
}

func TestProxyForwardsGuestStatus(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-err")

	_, err := pb.NewProcessClient(f.conn).Get(envCtx(t.Context(), "env-err"), &pb.GetRequest{
		ProcessId: "no-such-process",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Get unknown process = %v, want NotFound from the guest", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "no-such-process") {
		t.Fatalf("error %v lost the guest's message", err)
	}
}

func TestProxyRequiresEnvID(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-md")

	_, err := pb.NewProcessClient(f.conn).Exec(t.Context(), &pb.ExecRequest{Command: "true"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Exec without %s = %v, want InvalidArgument", grpcproxy.MetadataKey, err)
	}
}

// TestProxySuspendResume pins the data plane's central promise: a
// suspended environment fails calls with a retryable status, and the
// same client connection works again once it is running — no
// reconnect, no new handles.
func TestProxySuspendResume(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-sr")
	ctx := envCtx(t.Context(), "env-sr")
	procs := pb.NewProcessClient(f.conn)

	if _, err := procs.Exec(ctx, &pb.ExecRequest{Command: "true"}); err != nil {
		t.Fatalf("Exec while running: %v", err)
	}

	f.control.Suspend("env-sr")
	_, err := procs.Exec(ctx, &pb.ExecRequest{Command: "true"})
	if err == nil {
		t.Fatal("Exec against a suspended env succeeded, want an error")
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("Exec against a suspended env = %v (code %v), want Unavailable", err, code)
	}

	f.control.SetState("env-sr", ateapipb.ActorState_ACTOR_STATE_RUNNING)
	if _, err := procs.Exec(ctx, &pb.ExecRequest{Command: "true"}); err != nil {
		t.Fatalf("Exec after resume on the same connection: %v", err)
	}
}

// TestTypedServiceBesideProxy pins that the pass-through codec falls
// back to proto for services registered on the same server.
func TestTypedServiceBesideProxy(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-typed")

	resp, err := ateenvv1.NewEnvironmentServiceClient(f.conn).GetEnvironment(t.Context(), &ateenvv1.GetEnvironmentRequest{
		Id: "env-typed",
	})
	if err != nil {
		t.Fatalf("GetEnvironment on the proxying server: %v", err)
	}
	if resp.GetEnvironment().GetStatus() != ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING {
		t.Fatalf("status = %v, want RUNNING", resp.GetEnvironment().GetStatus())
	}
}
