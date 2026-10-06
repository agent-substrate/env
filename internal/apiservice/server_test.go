// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiservice_test

import (
	"context"
	"io"
	"math"
	"net"
	"testing"

	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type testEnv struct {
	envClient  ateenvv1alpha.EnvironmentServiceClient
	procClient ateenvv1alpha.ProcessServiceClient
	fsClient   ateenvv1alpha.FileSystemServiceClient
	conn       *grpc.ClientConn
	router     *fakerouter.Router
	control    *fakecontrol.Server
}

func newTestEnv(t *testing.T) (ateenvv1alpha.EnvironmentServiceClient, *fakecontrol.Server) {
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
		return control.Status(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
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
	ateenvv1alpha.RegisterEnvironmentServiceServer(grpcServer, srv)
	ateenvv1alpha.RegisterProcessServiceServer(grpcServer, srv)
	ateenvv1alpha.RegisterFileSystemServiceServer(grpcServer, srv)

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
		envClient:  ateenvv1alpha.NewEnvironmentServiceClient(conn),
		procClient: ateenvv1alpha.NewProcessServiceClient(conn),
		fsClient:   ateenvv1alpha.NewFileSystemServiceClient(conn),
		conn:       conn,
		router:     router,
		control:    control,
	}
}

func TestCreateAndGetEnvironment(t *testing.T) {
	client, _ := newTestEnv(t)
	ctx := context.Background()

	// Create with defaults.
	createResp, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
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
	if createResp.GetEnvironment().GetTemplate().GetAtespace() != apiservice.DefaultAtespace {
		t.Errorf("got template atespace %q, want %q", createResp.GetEnvironment().GetTemplate().GetAtespace(), apiservice.DefaultAtespace)
	}
	if createResp.GetEnvironment().GetAtespace() != apiservice.DefaultAtespace {
		t.Errorf("got atespace %q, want %q", createResp.GetEnvironment().GetAtespace(), apiservice.DefaultAtespace)
	}
	if createResp.GetEnvironment().GetStatus() != ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED {
		t.Errorf("got status %v, want UNSPECIFIED", createResp.GetEnvironment().GetStatus())
	}

	// Get environment.
	getResp, err := client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{
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
	if getResp.GetEnvironment().GetStatus() != ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING {
		t.Errorf("got status %v, want RUNNING", getResp.GetEnvironment().GetStatus())
	}
}

func TestCustomAtespace(t *testing.T) {
	client, _ := newTestEnv(t)
	ctx := context.Background()

	createResp, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
		Id:       "custom-env",
		Atespace: "my-space",
		Template: &ateenvv1alpha.Template{
			Name:     "custom-tmpl",
			Atespace: "my-space",
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
	if createResp.GetEnvironment().GetTemplate().GetAtespace() != "my-space" {
		t.Errorf("got template atespace %q, want my-space", createResp.GetEnvironment().GetTemplate().GetAtespace())
	}

	getResp, err := client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{
		Id:       "custom-env",
		Atespace: "my-space",
	})
	if err != nil {
		t.Fatalf("GetEnvironment failed: %v", err)
	}
	if getResp.GetEnvironment().GetAtespace() != "my-space" {
		t.Errorf("got atespace %q, want my-space", getResp.GetEnvironment().GetAtespace())
	}

	_, err = client.SuspendEnvironment(ctx, &ateenvv1alpha.SuspendEnvironmentRequest{
		Id:       "custom-env",
		Atespace: "my-space",
	})
	if err != nil {
		t.Fatalf("SuspendEnvironment failed: %v", err)
	}

	_, err = client.DeleteEnvironment(ctx, &ateenvv1alpha.DeleteEnvironmentRequest{
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

	_, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{})
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

	_, err := client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{
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

	_, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
		Id: "env-susp",
		Template: &ateenvv1alpha.Template{
			Name: "custom-template",
		},
	})
	if err != nil {
		t.Fatalf("CreateEnvironment failed: %v", err)
	}

	if got := control.Status("env-susp"); got != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("status before suspend = %v, want RUNNING", got)
	}

	suspResp, err := client.SuspendEnvironment(ctx, &ateenvv1alpha.SuspendEnvironmentRequest{
		Id: "env-susp",
	})
	if err != nil {
		t.Fatalf("SuspendEnvironment failed: %v", err)
	}
	if suspResp == nil {
		t.Fatal("expected non-nil SuspendEnvironmentResponse")
	}
	if got := control.Status("env-susp"); got != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("control plane status after suspend = %v, want SUSPENDED", got)
	}

	getResp, err := client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{
		Id: "env-susp",
	})
	if err != nil {
		t.Fatalf("GetEnvironment after suspend failed: %v", err)
	}
	if getResp.GetEnvironment().GetStatus() != ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED {
		t.Errorf("got status %v, want SUSPENDED", getResp.GetEnvironment().GetStatus())
	}
}

func TestDeleteEnvironment(t *testing.T) {
	client, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
		Id: "env-del",
	})
	if err != nil {
		t.Fatalf("CreateEnvironment failed: %v", err)
	}

	delResp, err := client.DeleteEnvironment(ctx, &ateenvv1alpha.DeleteEnvironmentRequest{
		Id: "env-del",
	})
	if err != nil {
		t.Fatalf("DeleteEnvironment failed: %v", err)
	}
	if delResp == nil {
		t.Fatal("expected non-nil DeleteEnvironmentResponse")
	}

	// Should no longer exist.
	_, err = client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{
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
	_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
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
	if err := writeStream.Send(&ateenvv1alpha.WriteFileRequest{
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

	readStream, err := te.fsClient.ReadFile(envCtx, &ateenvv1alpha.ReadFileRequest{
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
		readBuf = append(readBuf, chunk.GetChunk()...)
	}
	if string(readBuf) != "proxied content" {
		t.Errorf("read %q, want %q", string(readBuf), "proxied content")
	}

	// 4. Test ProcessService proxying
	startResp, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"echo", "hello-from-proxy"},
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	if startResp.GetProcessId() == "" {
		t.Fatal("empty process ID")
	}

	outStream, err := te.procClient.StreamProcessOutput(envCtx, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: startResp.GetProcessId(),
		Follow:    true,
	})
	if err != nil {
		t.Fatalf("StreamProcessOutput: %v", err)
	}
	var stdout string
	var exit *ateenvv1alpha.Process
	for {
		msg, err := outStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("StreamProcessOutput recv: %v", err)
		}
		switch out := msg.GetOutput().(type) {
		case *ateenvv1alpha.ProcessOutput_Stdout:
			stdout += string(out.Stdout)
		case *ateenvv1alpha.ProcessOutput_Exit:
			exit = out.Exit
		}
	}
	if stdout != "hello-from-proxy\n" {
		t.Errorf("stdout = %q, want hello-from-proxy\\n", stdout)
	}
	if exit == nil || exit.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED || exit.GetExitCode() != 0 {
		t.Errorf("exit message = %v, want EXITED with code 0", exit)
	}

	getProc, err := te.procClient.GetProcess(envCtx, &ateenvv1alpha.GetProcessRequest{
		ProcessId: startResp.GetProcessId(),
	})
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if getProc.GetExitCode() != 0 {
		t.Errorf("exit code = %d, want 0", getProc.GetExitCode())
	}

	// Stdin and signals proxy through the client stream and unary paths.
	catProc, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"cat"},
		Stdin:   true,
	})
	if err != nil {
		t.Fatalf("StartProcess cat: %v", err)
	}
	inStream, err := te.procClient.WriteProcessInput(envCtx)
	if err != nil {
		t.Fatalf("WriteProcessInput: %v", err)
	}
	if err := inStream.Send(&ateenvv1alpha.WriteProcessInputRequest{
		ProcessId: catProc.GetProcessId(),
		Data:      []byte("proxied stdin\n"),
		Close:     true,
	}); err != nil {
		t.Fatalf("WriteProcessInput send: %v", err)
	}
	inResp, err := inStream.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteProcessInput CloseAndRecv: %v", err)
	}
	if inResp.GetBytesWritten() != int64(len("proxied stdin\n")) {
		t.Errorf("bytes written = %d", inResp.GetBytesWritten())
	}
	catDone := waitProcess(t, te.procClient, envCtx, catProc.GetProcessId())
	if catDone.GetExitCode() != 0 {
		t.Errorf("cat exit code = %d", catDone.GetExitCode())
	}

	sleeper, err := te.procClient.StartProcess(envCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sleep", "30"}})
	if err != nil {
		t.Fatalf("StartProcess sleep: %v", err)
	}
	if _, err := te.procClient.SignalProcess(envCtx, &ateenvv1alpha.SignalProcessRequest{
		ProcessId: sleeper.GetProcessId(),
		Signal:    ateenvv1alpha.Signal_SIGNAL_TERM,
	}); err != nil {
		t.Fatalf("SignalProcess: %v", err)
	}
	sleeperDone := waitProcess(t, te.procClient, envCtx, sleeper.GetProcessId())
	if sleeperDone.GetExitCode() != 143 {
		t.Errorf("sleeper exit code = %d, want 143 (128+SIGTERM)", sleeperDone.GetExitCode())
	}

	// 5. Test missing metadata error
	_, err = te.procClient.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"echo", "no-meta"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("got error code %v, want InvalidArgument", status.Code(err))
	}
}

// waitProcess follows the output stream past the spool end and returns the exit message.
func waitProcess(t *testing.T, client ateenvv1alpha.ProcessServiceClient, ctx context.Context, id string) *ateenvv1alpha.Process {
	t.Helper()
	stream, err := client.StreamProcessOutput(ctx, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: id, Follow: true, StdoutOffset: math.MaxInt64, StderrOffset: math.MaxInt64,
	})
	if err != nil {
		t.Fatalf("StreamProcessOutput: %v", err)
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("waiting for %s: %v", id, err)
		}
		if exit := msg.GetExit(); exit != nil {
			return exit
		}
		t.Fatalf("unexpected output while waiting: %v", msg)
	}
}

func TestActorStatusToEnvStatus(t *testing.T) {
	cases := []struct {
		name string
		in   *ateapipb.ActorStatus
		want ateenvv1alpha.EnvironmentStatus
	}{
		{"nil status", nil, ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED},
		{"resuming", &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RESUMING}, ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RESUMING},
		{"running", &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}, ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING},
		{"suspending", &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING}, ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDING},
		{"suspended", &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}, ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED},
		{"pausing", &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_PAUSING}, ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_PAUSING},
		{"paused", &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_PAUSED}, ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_PAUSED},
		{"crashed", &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED}, ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_CRASHED},
		{"deleting", &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_DELETING}, ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_DELETING},
	}
	for _, tc := range cases {
		if got := apiservice.ActorStatusToEnvStatus(tc.in); got != tc.want {
			t.Errorf("%s: ActorStatusToEnvStatus = %v, want %v", tc.name, got, tc.want)
		}
	}
}
