package grpcproxy_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/grpcproxy"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fixture runs the full data plane the proxy sits in: guests behind a
// fake router, an ate.Client on a fake control plane, and an api-side
// gRPC server forwarding unknown methods to guests.
type fixture struct {
	control *fakecontrol.Server
	router  *fakerouter.Router
	client  *ate.Client
	conn    *grpc.ClientConn
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

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

	grpcServer := grpc.NewServer(
		grpc.ForceServerCodec(grpcproxy.Codec()),
		grpc.UnknownServiceHandler(grpcproxy.StreamHandler(
			func(ctx context.Context, atespace, envID string) (*grpc.ClientConn, error) {
				return client.GuestConn(atespace, envID)
			},
		)),
	)
	ateenvv1.RegisterEnvironmentServiceServer(grpcServer, apiservice.New(client))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	return &fixture{control: control, router: router, client: client, conn: conn}
}

// createEnv registers a real guest server behind the fake router and
// creates the backing actor.
func (f *fixture) createEnv(t *testing.T, id string) {
	t.Helper()
	guestGrpc, cleanup, err := guest.NewServer(guest.Config{
		LogDir:           t.TempDir(),
		Workspace:        t.TempDir(),
		EnableProcess:    true,
		EnableFileSystem: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(guestGrpc.Stop)
	f.router.Register(id, guestGrpc)

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

// shell runs a command through the proxied data plane to completion and
// returns its combined stdout.
func shell(t *testing.T, ctx context.Context, conn *grpc.ClientConn, command string) string {
	t.Helper()
	procs := ateenvv1.NewProcessServiceClient(conn)
	start, err := procs.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", command},
	})
	if err != nil {
		t.Fatalf("StartProcess(%q): %v", command, err)
	}
	var out strings.Builder
	var stdoutOff, stderrOff int64
	for {
		proc, err := procs.GetProcess(ctx, &ateenvv1.GetProcessRequest{ProcessId: start.GetProcessId()})
		if err != nil {
			t.Fatalf("GetProcess: %v", err)
		}
		running := proc.GetStatus() == ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING

		stream, err := procs.StreamProcessLogs(ctx, &ateenvv1.StreamProcessLogsRequest{
			ProcessId:    start.GetProcessId(),
			StdoutOffset: stdoutOff,
			StderrOffset: stderrOff,
			WaitMs:       2000,
		})
		if err != nil {
			t.Fatalf("StreamProcessLogs: %v", err)
		}
		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("log recv: %v", err)
			}
			switch chunk.GetSource() {
			case ateenvv1.LogSource_LOG_SOURCE_STDOUT:
				out.Write(chunk.GetData())
				stdoutOff += int64(len(chunk.GetData()))
			case ateenvv1.LogSource_LOG_SOURCE_STDERR:
				stderrOff += int64(len(chunk.GetData()))
			}
		}
		if !running {
			return out.String()
		}
	}
}

func TestProxyProcessRoundtrip(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-1")

	out := shell(t, envCtx(t.Context(), "env-1"), f.conn, "printf 'through the proxy'")
	if out != "through the proxy" {
		t.Fatalf("shell output = %q, want %q", out, "through the proxy")
	}
}

func TestProxyFileStreaming(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-fs")
	ctx := envCtx(t.Context(), "env-fs")
	fs := ateenvv1.NewFileSystemServiceClient(f.conn)

	content := strings.Repeat("0123456789abcdef", 16<<10) // 256 KiB
	w, err := fs.WriteFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(&ateenvv1.WriteFileRequest{Path: "data.bin", Chunk: []byte(content[:100<<10])}); err != nil {
		t.Fatal(err)
	}
	if err := w.Send(&ateenvv1.WriteFileRequest{Chunk: []byte(content[100<<10:])}); err != nil {
		t.Fatal(err)
	}
	wres, err := w.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteFile through proxy: %v", err)
	}
	if wres.GetBytesWritten() != int64(len(content)) {
		t.Fatalf("BytesWritten = %d, want %d", wres.GetBytesWritten(), len(content))
	}

	r, err := fs.ReadFile(ctx, &ateenvv1.ReadFileRequest{Path: "data.bin"})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	chunks := 0
	for {
		resp, err := r.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFile through proxy: %v", err)
		}
		chunks++
		got.Write(resp.GetData())
	}
	if got.String() != content {
		t.Fatalf("read back %d bytes, want %d", got.Len(), len(content))
	}
	if chunks < 2 {
		t.Fatalf("read back in %d chunk(s), want a real stream", chunks)
	}
}

func TestProxyForwardsGuestStatus(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-err")

	_, err := ateenvv1.NewProcessServiceClient(f.conn).GetProcess(envCtx(t.Context(), "env-err"), &ateenvv1.GetProcessRequest{
		ProcessId: "no-such-process",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetProcess unknown process = %v, want NotFound from the guest", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "no-such-process") {
		t.Fatalf("error %v lost the guest's message", err)
	}
}

func TestProxyRequiresEnvID(t *testing.T) {
	f := newFixture(t)
	f.createEnv(t, "env-md")

	_, err := ateenvv1.NewProcessServiceClient(f.conn).GetProcess(t.Context(), &ateenvv1.GetProcessRequest{ProcessId: "x"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("GetProcess without %s = %v, want InvalidArgument", grpcproxy.MetadataKey, err)
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

	if out := shell(t, ctx, f.conn, "echo alive"); !strings.Contains(out, "alive") {
		t.Fatalf("shell while running = %q", out)
	}

	f.control.Suspend("env-sr")
	_, err := ateenvv1.NewProcessServiceClient(f.conn).GetProcess(ctx, &ateenvv1.GetProcessRequest{ProcessId: "p"})
	if err == nil {
		t.Fatal("call against a suspended env succeeded, want an error")
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("call against a suspended env = %v (code %v), want Unavailable", err, code)
	}

	f.control.SetState("env-sr", ateapipb.ActorState_ACTOR_STATE_RUNNING)
	if out := shell(t, ctx, f.conn, "echo back"); !strings.Contains(out, "back") {
		t.Fatalf("shell after resume on the same connection = %q", out)
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