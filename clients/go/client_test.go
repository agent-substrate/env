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

package env_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/clients/go"
	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/mcp"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
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
		return control.Status(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
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
	ateenvv1alpha.RegisterEnvironmentServiceServer(grpcServer, service)
	ateenvv1alpha.RegisterProcessServiceServer(grpcServer, service)
	ateenvv1alpha.RegisterFileSystemServiceServer(grpcServer, service)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/v1alpha/envs/{id}/mcp", mcp.NewHandler(directClient))
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

	sb, err := f.client.Create(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{
		Id: id,
		Template: &ateenvv1alpha.Template{
			Name:     "default-template",
			Atespace: "envs",
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

	if st := f.control.Status("sb-susp"); st != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
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

func TestWriteFileAt(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-seek")
	ctx := t.Context()

	if err := sb.WriteFile(ctx, "seek.txt", strings.NewReader("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFileAt(ctx, "seek.txt", 6, strings.NewReader("W"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, err := sb.ReadFile(ctx, "seek.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello World" {
		t.Errorf("after WriteFileAt: %q, want %q", data, "hello World")
	}
	if err := sb.WriteFileAt(ctx, "seek.txt", -1, strings.NewReader("x"), 0o644); err == nil {
		t.Error("negative offset should be rejected")
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

func TestRunWithStdin(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-stdin")
	ctx := t.Context()

	res, err := sb.Run(ctx, env.ShellRequest{Command: "tr a-z A-Z", Stdin: []byte("shout\n")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "SHOUT\n" || res.ExitCode != 0 {
		t.Errorf("run result = %+v, want stdout %q", res, "SHOUT\n")
	}
}

func TestProcessInteractiveStdinAndOutput(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-proc")
	ctx := t.Context()

	proc, err := sb.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"cat"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := proc.Output(ctx, &ateenvv1alpha.StreamProcessOutputRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}

	stdin, err := proc.Stdin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	out, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if string(out.GetStdout()) != "first\n" {
		t.Fatalf("first output = %v", out)
	}
	if _, err := stdin.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}

	var rest string
	var exit *ateenvv1alpha.Process
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rest += string(out.GetStdout())
		if out.GetExit() != nil {
			exit = out.GetExit()
		}
	}
	if rest != "second\n" {
		t.Errorf("remaining output = %q", rest)
	}
	if exit == nil || exit.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED || exit.GetExitCode() != 0 {
		t.Errorf("exit = %v, want exited with 0", exit)
	}

	// The handle still resolves after exit; stdin is refused.
	info, err := sb.FindProcess(ctx, proc.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED || info.GetPid() == 0 || len(info.GetCommand()) != 1 {
		t.Errorf("info = %v", info)
	}
	w, err := proc.Stdin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("late"))
	if err := w.Close(); !errors.Is(err, env.ErrProcessExited) {
		t.Errorf("stdin after exit: err = %v, want ErrProcessExited", err)
	}
}

func TestProcessSignalAndKill(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-sig")
	ctx := t.Context()

	proc, err := sb.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(ctx, ateenvv1alpha.Signal_SIGNAL_TERM); err != nil {
		t.Fatal(err)
	}
	info, err := proc.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED || info.GetExitCode() != 143 {
		t.Errorf("after SIGTERM: %v", info)
	}
	if err := proc.Signal(ctx, ateenvv1alpha.Signal_SIGNAL_KILL); !errors.Is(err, env.ErrProcessExited) {
		t.Errorf("signal after exit: err = %v, want ErrProcessExited", err)
	}

	sleeper, err := sb.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	killed, err := sleeper.Kill(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if killed.GetExitCode() != 137 {
		t.Errorf("kill: %v", killed)
	}
	// Kill is idempotent.
	if _, err := sleeper.Kill(ctx); err != nil {
		t.Errorf("second kill: %v", err)
	}

	if _, err := sb.FindProcess(ctx, "bogus"); !errors.Is(err, env.ErrNotFound) {
		t.Errorf("bogus process: err = %v, want ErrNotFound", err)
	}
}

func TestShellKilledByTimeout(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-timeout")

	res, err := sb.Run(t.Context(), env.ShellRequest{Command: "sleep 30", Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 137 {
		t.Errorf("timed out run = %+v, want exit code 137", res)
	}
}
