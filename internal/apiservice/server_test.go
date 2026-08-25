package apiservice_test

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
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

type testEnv struct {
	envClient  ateenvv1.EnvironmentServiceClient
	procClient ateenvv1.ProcessServiceClient
	fsClient   ateenvv1.FileSystemServiceClient
	conn       *grpc.ClientConn
	router     *fakerouter.Router
	control    *fakecontrol.Server
}

func newTestEnv(t *testing.T) (ateenvv1.EnvironmentServiceClient, *fakecontrol.Server) {
	t.Helper()
	te := newFullTestEnv(t)
	return te.envClient, te.control
}

func newFullTestEnv(t *testing.T) *testEnv {
	t.Helper()

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.Status(id) == ateapipb.Actor_STATUS_RUNNING
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

	srv := apiservice.New(client, routerAddr, "")
	t.Cleanup(srv.Close)

	grpcServer := grpc.NewServer()
	ateenvv1.RegisterEnvironmentServiceServer(grpcServer, srv)
	ateenvv1.RegisterProcessServiceServer(grpcServer, srv)
	ateenvv1.RegisterFileSystemServiceServer(grpcServer, srv)

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

	return &testEnv{
		envClient:  ateenvv1.NewEnvironmentServiceClient(conn),
		procClient: ateenvv1.NewProcessServiceClient(conn),
		fsClient:   ateenvv1.NewFileSystemServiceClient(conn),
		conn:       conn,
		router:     router,
		control:    control,
	}
}

func TestCreateAndGetEnvironment(t *testing.T) {
	client, _ := newTestEnv(t)
	ctx := context.Background()

	// Create with defaults.
	createResp, err := client.CreateEnvironment(ctx, &ateenvv1.CreateEnvironmentRequest{
		Id: "env-1",
	})
	if err != nil {
		t.Fatalf("CreateEnvironment failed: %v", err)
	}
	if createResp.GetEnvironment().GetId() != "env-1" {
		t.Errorf("got id %q, want env-1", createResp.GetEnvironment().GetId())
	}
	if createResp.GetEnvironment().GetTemplate().GetName() != apiservice.DefaultTemplate {
		t.Errorf("got template name %q, want %q", createResp.GetEnvironment().GetTemplate().GetName(), apiservice.DefaultTemplate)
	}
	if createResp.GetEnvironment().GetTemplate().GetNamespace() != apiservice.DefaultNamespace {
		t.Errorf("got template namespace %q, want %q", createResp.GetEnvironment().GetTemplate().GetNamespace(), apiservice.DefaultNamespace)
	}
	if createResp.GetEnvironment().GetAtespace() != apiservice.DefaultAtespace {
		t.Errorf("got atespace %q, want %q", createResp.GetEnvironment().GetAtespace(), apiservice.DefaultAtespace)
	}
	if createResp.GetEnvironment().GetStatus() != ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED {
		t.Errorf("got status %v, want UNSPECIFIED", createResp.GetEnvironment().GetStatus())
	}

	// Get environment.
	getResp, err := client.GetEnvironment(ctx, &ateenvv1.GetEnvironmentRequest{
		Id: "env-1",
	})
	if err != nil {
		t.Fatalf("GetEnvironment failed: %v", err)
	}
	if getResp.GetEnvironment().GetId() != "env-1" {
		t.Errorf("got id %q, want env-1", getResp.GetEnvironment().GetId())
	}
	if getResp.GetEnvironment().GetTemplate().GetName() != apiservice.DefaultTemplate {
		t.Errorf("got template name %q, want %q", getResp.GetEnvironment().GetTemplate().GetName(), apiservice.DefaultTemplate)
	}
	if getResp.GetEnvironment().GetAtespace() != apiservice.DefaultAtespace {
		t.Errorf("got atespace %q, want %q", getResp.GetEnvironment().GetAtespace(), apiservice.DefaultAtespace)
	}
	if getResp.GetEnvironment().GetStatus() != ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING {
		t.Errorf("got status %v, want RUNNING", getResp.GetEnvironment().GetStatus())
	}
}

func TestCustomAtespace(t *testing.T) {
	client, _ := newTestEnv(t)
	ctx := context.Background()

	createResp, err := client.CreateEnvironment(ctx, &ateenvv1.CreateEnvironmentRequest{
		Id:       "custom-env",
		Atespace: "my-space",
		Template: &ateenvv1.Template{
			Name:      "custom-tmpl",
			Namespace: "custom-ns",
		},
	})
	if err != nil {
		t.Fatalf("CreateEnvironment failed: %v", err)
	}
	if createResp.GetEnvironment().GetAtespace() != "my-space" {
		t.Errorf("got atespace %q, want my-space", createResp.GetEnvironment().GetAtespace())
	}
	if createResp.GetEnvironment().GetTemplate().GetName() != "custom-tmpl" {
		t.Errorf("got template name %q, want custom-tmpl", createResp.GetEnvironment().GetTemplate().GetName())
	}
	if createResp.GetEnvironment().GetTemplate().GetNamespace() != "custom-ns" {
		t.Errorf("got template namespace %q, want custom-ns", createResp.GetEnvironment().GetTemplate().GetNamespace())
	}

	getResp, err := client.GetEnvironment(ctx, &ateenvv1.GetEnvironmentRequest{
		Id:       "custom-env",
		Atespace: "my-space",
	})
	if err != nil {
		t.Fatalf("GetEnvironment failed: %v", err)
	}
	if getResp.GetEnvironment().GetAtespace() != "my-space" {
		t.Errorf("got atespace %q, want my-space", getResp.GetEnvironment().GetAtespace())
	}

	_, err = client.SuspendEnvironment(ctx, &ateenvv1.SuspendEnvironmentRequest{
		Id:       "custom-env",
		Atespace: "my-space",
	})
	if err != nil {
		t.Fatalf("SuspendEnvironment failed: %v", err)
	}

	_, err = client.DeleteEnvironment(ctx, &ateenvv1.DeleteEnvironmentRequest{
		Id:       "custom-env",
		Atespace: "my-space",
	})
	if err != nil {
		t.Fatalf("DeleteEnvironment failed: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	client, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := client.CreateEnvironment(ctx, &ateenvv1.CreateEnvironmentRequest{})
	if err == nil {
		t.Fatal("expected error for empty id")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("got code %v, want %v", status.Code(err), codes.InvalidArgument)
	}
}

func TestGetNotFound(t *testing.T) {
	client, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := client.GetEnvironment(ctx, &ateenvv1.GetEnvironmentRequest{
		Id: "nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent environment")
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("got code %v, want %v", status.Code(err), codes.NotFound)
	}
}

func TestSuspendEnvironment(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()

	_, err := client.CreateEnvironment(ctx, &ateenvv1.CreateEnvironmentRequest{
		Id: "env-susp",
		Template: &ateenvv1.Template{
			Name:      "custom-template",
			Namespace: "custom-ns",
		},
	})
	if err != nil {
		t.Fatalf("CreateEnvironment failed: %v", err)
	}

	if got := control.Status("env-susp"); got != ateapipb.Actor_STATUS_RUNNING {
		t.Fatalf("status before suspend = %v, want RUNNING", got)
	}

	suspResp, err := client.SuspendEnvironment(ctx, &ateenvv1.SuspendEnvironmentRequest{
		Id: "env-susp",
	})
	if err != nil {
		t.Fatalf("SuspendEnvironment failed: %v", err)
	}
	if suspResp == nil {
		t.Fatal("expected non-nil SuspendEnvironmentResponse")
	}
	if got := control.Status("env-susp"); got != ateapipb.Actor_STATUS_SUSPENDED {
		t.Errorf("control plane status after suspend = %v, want SUSPENDED", got)
	}

	getResp, err := client.GetEnvironment(ctx, &ateenvv1.GetEnvironmentRequest{
		Id: "env-susp",
	})
	if err != nil {
		t.Fatalf("GetEnvironment after suspend failed: %v", err)
	}
	if getResp.GetEnvironment().GetStatus() != ateenvv1.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED {
		t.Errorf("got status %v, want SUSPENDED", getResp.GetEnvironment().GetStatus())
	}
}

func TestDeleteEnvironment(t *testing.T) {
	client, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := client.CreateEnvironment(ctx, &ateenvv1.CreateEnvironmentRequest{
		Id: "env-del",
	})
	if err != nil {
		t.Fatalf("CreateEnvironment failed: %v", err)
	}

	delResp, err := client.DeleteEnvironment(ctx, &ateenvv1.DeleteEnvironmentRequest{
		Id: "env-del",
	})
	if err != nil {
		t.Fatalf("DeleteEnvironment failed: %v", err)
	}
	if delResp == nil {
		t.Fatal("expected non-nil DeleteEnvironmentResponse")
	}

	// Should no longer exist.
	_, err = client.GetEnvironment(ctx, &ateenvv1.GetEnvironmentRequest{
		Id: "env-del",
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("after delete, got code %v, want %v", status.Code(err), codes.NotFound)
	}
}

func TestProxyGuestServices(t *testing.T) {
	te := newFullTestEnv(t)
	ctx := context.Background()

	// 1. Create environment
	_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1.CreateEnvironmentRequest{
		Id: "guest-test",
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// 2. Register guest server on router
	workDir := t.TempDir()
	grpcGuestServer, cleanup, err := guest.NewServer(guest.Config{
		Workspace:        workDir,
		EnableProcess:    true,
		EnableFileSystem: true,
	})
	if err != nil {
		t.Fatalf("guest.NewServer: %v", err)
	}
	t.Cleanup(cleanup)

	te.router.Register("guest-test", grpcGuestServer)

	envCtx := metadata.AppendToOutgoingContext(ctx, "x-env-id", "guest-test", "x-env-atespace", "default")

	// 3. Test FileSystemService proxying
	writeStream, err := te.fsClient.WriteFile(envCtx)
	if err != nil {
		t.Fatalf("WriteFile stream: %v", err)
	}
	if err := writeStream.Send(&ateenvv1.WriteFileRequest{
		Path:  "proxy-file.txt",
		Mode:  0644,
		Chunk: []byte("proxied content"),
	}); err != nil {
		t.Fatalf("WriteFile send: %v", err)
	}
	writeResp, err := writeStream.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteFile CloseAndRecv: %v", err)
	}
	if writeResp.GetBytesWritten() != int64(len("proxied content")) {
		t.Errorf("bytes written = %d, want %d", writeResp.GetBytesWritten(), len("proxied content"))
	}

	readStream, err := te.fsClient.ReadFile(envCtx, &ateenvv1.ReadFileRequest{
		Path: "proxy-file.txt",
	})
	if err != nil {
		t.Fatalf("ReadFile stream: %v", err)
	}
	var readBuf []byte
	for {
		chunk, err := readStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadFile recv: %v", err)
		}
		readBuf = append(readBuf, chunk.GetData()...)
	}
	if string(readBuf) != "proxied content" {
		t.Errorf("read %q, want %q", string(readBuf), "proxied content")
	}

	// 4. Test ProcessService proxying
	startResp, err := te.procClient.StartProcess(envCtx, &ateenvv1.StartProcessRequest{
		Command: []string{"echo", "hello-from-proxy"},
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	if startResp.GetProcessId() == "" {
		t.Fatal("empty process ID")
	}

	logStream, err := te.procClient.StreamProcessLogs(envCtx, &ateenvv1.StreamProcessLogsRequest{
		ProcessId: startResp.GetProcessId(),
		Follow:    true,
	})
	if err != nil {
		t.Fatalf("StreamProcessLogs: %v", err)
	}
	var stdout string
	for {
		chunk, err := logStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("StreamProcessLogs recv: %v", err)
		}
		if chunk.GetSource() == ateenvv1.LogSource_LOG_SOURCE_STDOUT {
			stdout += string(chunk.GetData())
		}
	}
	if stdout != "hello-from-proxy\n" {
		t.Errorf("stdout = %q, want hello-from-proxy\\n", stdout)
	}

	getProc, err := te.procClient.GetProcess(envCtx, &ateenvv1.GetProcessRequest{
		ProcessId: startResp.GetProcessId(),
	})
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if getProc.GetExitCode() != 0 {
		t.Errorf("exit code = %d, want 0", getProc.GetExitCode())
	}

	// 5. Test missing metadata error
	_, err = te.procClient.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"echo", "no-meta"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("got error code %v, want InvalidArgument", status.Code(err))
	}
}
