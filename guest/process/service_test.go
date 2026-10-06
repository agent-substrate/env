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

package process

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
)

func setupTestServer(t *testing.T) ateenvv1alpha.ProcessServiceClient {
	t.Helper()
	return setupTestServerWithConfig(t, DefaultConfig(t.TempDir()))
}

func setupTestServerWithConfig(t *testing.T, cfg TrackerConfig) ateenvv1alpha.ProcessServiceClient {
	t.Helper()
	if cfg.LogDir == "" {
		cfg.LogDir = t.TempDir()
	}

	tracker, err := NewTracker(cfg)
	if err != nil {
		t.Fatalf("failed to create tracker: %v", err)
	}

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	ateenvv1alpha.RegisterProcessServiceServer(server, NewService(tracker))
	go func() { _ = server.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}

	t.Cleanup(func() {
		tracker.Close()
		conn.Close()
		server.Stop()
		lis.Close()
		_ = os.RemoveAll(cfg.LogDir)
	})
	return ateenvv1alpha.NewProcessServiceClient(conn)
}

func start(t *testing.T, client ateenvv1alpha.ProcessServiceClient, req *ateenvv1alpha.StartProcessRequest) *ateenvv1alpha.Process {
	t.Helper()
	proc, err := client.StartProcess(context.Background(), req)
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}
	if proc.GetProcessId() == "" || proc.GetPid() == 0 {
		t.Fatalf("expected process id and pid, got %v", proc)
	}
	if proc.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING {
		t.Fatalf("expected RUNNING right after start, got %v", proc.GetState())
	}
	return proc
}

func sh(t *testing.T, client ateenvv1alpha.ProcessServiceClient, script string) *ateenvv1alpha.Process {
	t.Helper()
	return start(t, client, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", script}})
}

// wait follows the output stream with offsets past the spool end, so only
// the exit message is delivered.
func wait(t *testing.T, client ateenvv1alpha.ProcessServiceClient, id string) *ateenvv1alpha.Process {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.StreamProcessOutput(ctx, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: id, Follow: true, StdoutOffset: math.MaxInt64, StderrOffset: math.MaxInt64,
	})
	if err != nil {
		t.Fatalf("StreamProcessOutput failed: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("waiting for process: %v", err)
	}
	proc := msg.GetExit()
	if proc == nil {
		t.Fatalf("expected only an exit message with offsets past the end, got %v", msg)
	}
	if proc.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
		t.Fatalf("expected EXITED after wait, got %v", proc.GetState())
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after exit, got %v", err)
	}
	return proc
}

// collect drains an output stream into stdout, stderr and the final exit message.
func collect(t *testing.T, client ateenvv1alpha.ProcessServiceClient, req *ateenvv1alpha.StreamProcessOutputRequest) (string, string, *ateenvv1alpha.Process) {
	t.Helper()
	stream, err := client.StreamProcessOutput(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamProcessOutput failed: %v", err)
	}
	var stdout, stderr strings.Builder
	var exit *ateenvv1alpha.Process
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stdout.String(), stderr.String(), exit
		}
		if err != nil {
			t.Fatalf("stream recv: %v", err)
		}
		switch out := msg.GetOutput().(type) {
		case *ateenvv1alpha.ProcessOutput_Stdout:
			stdout.Write(out.Stdout)
		case *ateenvv1alpha.ProcessOutput_Stderr:
			stderr.Write(out.Stderr)
		case *ateenvv1alpha.ProcessOutput_Exit:
			if exit != nil {
				t.Fatalf("received two exit messages")
			}
			exit = out.Exit
		default:
			t.Fatalf("unexpected output %T", out)
		}
		if exit != nil {
			// exit must be the last message.
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("expected EOF after exit message, got %v", err)
			}
			return stdout.String(), stderr.String(), exit
		}
	}
}

func TestStartAndWait(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, "echo 'hello from substrate'")
	if got := started.GetCommand(); len(got) != 3 || got[0] != "sh" {
		t.Fatalf("command not echoed back: %v", got)
	}

	proc := wait(t, client, started.GetProcessId())
	if proc.GetExitCode() != 0 {
		t.Fatalf("expected exit_code 0, got %d", proc.GetExitCode())
	}
	if proc.GetStartedAt() == nil || proc.GetFinishedAt() == nil {
		t.Fatalf("expected non-nil timestamps")
	}

	got, err := client.GetProcess(context.Background(), &ateenvv1alpha.GetProcessRequest{ProcessId: started.GetProcessId()})
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if got.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
		t.Fatalf("GetProcess state = %v, want EXITED", got.GetState())
	}
}

func TestNonZeroExitCode(t *testing.T) {
	client := setupTestServer(t)
	proc := wait(t, client, sh(t, client, "exit 42").GetProcessId())
	if proc.GetExitCode() != 42 {
		t.Fatalf("expected exit_code 42, got %d", proc.GetExitCode())
	}
}

func TestSelfSignalDeath(t *testing.T) {
	client := setupTestServer(t)
	proc := wait(t, client, sh(t, client, "kill -15 $$").GetProcessId())
	if proc.GetExitCode() != 143 {
		t.Fatalf("expected exit_code 143 (128+SIGTERM), got %d", proc.GetExitCode())
	}
}

func TestStreamOutputFollowEndsWithExit(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, "echo 'out1'; echo 'err1' >&2; sleep 0.1; echo 'out2'; exit 3")

	stdout, stderr, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: started.GetProcessId(), Follow: true,
	})
	if stdout != "out1\nout2\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "err1\n" {
		t.Fatalf("stderr = %q", stderr)
	}
	if exit == nil || exit.GetExitCode() != 3 {
		t.Fatalf("expected exit message with code 3, got %v", exit)
	}
}

func TestStreamOutputOffsets(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, "printf abcdef; printf 123456 >&2")
	wait(t, client, started.GetProcessId())

	stdout, stderr, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: started.GetProcessId(), StdoutOffset: 4, StderrOffset: 2,
	})
	if stdout != "ef" || stderr != "3456" {
		t.Fatalf("got stdout %q stderr %q", stdout, stderr)
	}
	if exit == nil {
		t.Fatalf("snapshot of an exited process should end with exit")
	}
}

func TestStreamOutputSnapshotOfRunningProcess(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, "echo 'instant-output'; sleep 5")
	time.Sleep(100 * time.Millisecond)

	begin := time.Now()
	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId()})
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("snapshot took %v, should return immediately", d)
	}
	if stdout != "instant-output\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if exit != nil {
		t.Fatalf("running process must not produce an exit message")
	}
	_, _ = client.SignalProcess(context.Background(), &ateenvv1alpha.SignalProcessRequest{
		ProcessId: started.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_KILL,
	})
}

func TestSignalProcess(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	started := sh(t, client, "sleep 60")

	proc, err := client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{
		ProcessId: started.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_TERM,
	})
	if err != nil {
		t.Fatalf("SignalProcess failed: %v", err)
	}
	if proc.GetProcessId() != started.GetProcessId() {
		t.Fatalf("SignalProcess returned wrong process %v", proc)
	}

	final := wait(t, client, started.GetProcessId())
	if final.GetExitCode() != 143 {
		t.Fatalf("expected exit_code 143 (128+SIGTERM), got %d", final.GetExitCode())
	}

	// Signalling an exited process is a failed precondition.
	_, err = client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{
		ProcessId: started.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_KILL,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	// Unspecified signal is rejected.
	_, err = client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: started.GetProcessId()})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestSignalTrappedByProcess(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, `trap 'echo got-usr1; exit 7' USR1; while :; do sleep 0.05; done`)
	time.Sleep(150 * time.Millisecond)

	if _, err := client.SignalProcess(context.Background(), &ateenvv1alpha.SignalProcessRequest{
		ProcessId: started.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_USR1,
	}); err != nil {
		t.Fatalf("SignalProcess failed: %v", err)
	}

	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId(), Follow: true})
	if !strings.Contains(stdout, "got-usr1") {
		t.Fatalf("handler did not run, stdout = %q", stdout)
	}
	if exit.GetExitCode() != 7 {
		t.Fatalf("expected exit code 7 from handler, got %d", exit.GetExitCode())
	}
}

func TestStopAndContinue(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	started := sh(t, client, "sleep 0.2; echo done")

	signal := func(sig ateenvv1alpha.Signal) {
		if _, err := client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: started.GetProcessId(), Signal: sig}); err != nil {
			t.Fatalf("signal %v: %v", sig, err)
		}
	}
	signal(ateenvv1alpha.Signal_SIGNAL_STOP)

	// Well past the sleep: a stopped process must not have progressed.
	time.Sleep(500 * time.Millisecond)
	proc, err := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: started.GetProcessId()})
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if proc.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING {
		t.Fatalf("stopped process should still be RUNNING, got %v", proc.GetState())
	}

	signal(ateenvv1alpha.Signal_SIGNAL_CONT)
	if final := wait(t, client, started.GetProcessId()); final.GetExitCode() != 0 {
		t.Fatalf("expected clean exit after CONT, got %d", final.GetExitCode())
	}
}

func TestStdinStreaming(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{Command: []string{"cat"}, Stdin: true})

	in, err := client.WriteProcessInput(ctx)
	if err != nil {
		t.Fatalf("WriteProcessInput: %v", err)
	}
	if err := in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: started.GetProcessId(), Data: []byte("hello ")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := in.Send(&ateenvv1alpha.WriteProcessInputRequest{Data: []byte("world\n")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, err := in.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	if resp.GetBytesWritten() != int64(len("hello world\n")) {
		t.Fatalf("bytes_written = %d", resp.GetBytesWritten())
	}

	// cat is still running: stdin was not closed. Snapshot shows the echo.
	deadline := time.Now().Add(2 * time.Second)
	for {
		stdout, _, _ := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId()})
		if stdout == "hello world\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cat did not echo input, stdout = %q", stdout)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p, _ := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: started.GetProcessId()}); p.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING {
		t.Fatalf("cat should still be running with stdin open")
	}

	// Second call: more data plus EOF.
	in, err = client.WriteProcessInput(ctx)
	if err != nil {
		t.Fatalf("WriteProcessInput: %v", err)
	}
	_ = in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: started.GetProcessId(), Data: []byte("bye\n"), Close: true})
	if _, err := in.CloseAndRecv(); err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}

	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId(), Follow: true})
	if stdout != "hello world\nbye\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if exit.GetExitCode() != 0 {
		t.Fatalf("cat exit = %d", exit.GetExitCode())
	}

	// Writing after close fails.
	in, _ = client.WriteProcessInput(ctx)
	_ = in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: started.GetProcessId(), Data: []byte("late")})
	if _, err := in.CloseAndRecv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition writing after close, got %v", err)
	}
}

func TestStdinRejectedWhenNotRequested(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{Command: []string{"cat"}})

	// Without a stdin pipe cat sees EOF immediately and exits 0.
	if proc := wait(t, client, started.GetProcessId()); proc.GetExitCode() != 0 {
		t.Fatalf("cat without stdin should exit 0, got %d", proc.GetExitCode())
	}

	in, _ := client.WriteProcessInput(ctx)
	_ = in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: started.GetProcessId(), Data: []byte("x")})
	if _, err := in.CloseAndRecv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	in, _ = client.WriteProcessInput(ctx)
	_ = in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: "nope", Data: []byte("x")})
	if _, err := in.CloseAndRecv(); status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}

	in, _ = client.WriteProcessInput(ctx)
	if _, err := in.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty stream, got %v", err)
	}
}

func TestNotFound(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	if _, err := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetProcess: expected NotFound, got %v", err)
	}
	stream, err := client.StreamProcessOutput(ctx, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: "nope"})
	if err != nil {
		t.Fatalf("StreamProcessOutput: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Fatalf("StreamProcessOutput: expected NotFound, got %v", err)
	}
	if _, err := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty id: expected InvalidArgument, got %v", err)
	}
	if _, err := client.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty command: expected InvalidArgument, got %v", err)
	}
}

func TestConcurrencyLimiter(t *testing.T) {
	cfg := DefaultConfig("")
	cfg.MaxConcurrentProcesses = 2
	client := setupTestServerWithConfig(t, cfg)
	ctx := context.Background()

	res1 := sh(t, client, "sleep 5")
	res2 := sh(t, client, "sleep 5")

	_, err := client.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", "echo 'should fail'"}})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted for job 3, got: %v", err)
	}

	// Kill job 1 and wait for the slot to free up.
	if _, err := client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: res1.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_KILL}); err != nil {
		t.Fatalf("failed to kill job 1: %v", err)
	}
	if proc := wait(t, client, res1.GetProcessId()); proc.GetExitCode() != 137 {
		t.Fatalf("expected 137 (128+SIGKILL), got %d", proc.GetExitCode())
	}

	sh(t, client, "echo 'now succeeds'")
	_, _ = client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: res2.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_KILL})
}

func TestLogCapping(t *testing.T) {
	cfg := DefaultConfig("")
	cfg.MaxLogBytes = 256
	client := setupTestServerWithConfig(t, cfg)

	started := sh(t, client, "for i in $(seq 1 500); do echo 'spamming-log-line-0123456789'; done")
	stdout, _, _ := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId(), Follow: true})

	if !strings.Contains(stdout, "maximum log limit") || !strings.Contains(stdout, "truncated") {
		t.Fatalf("expected truncation warning in capped logs, got:\n%s", stdout)
	}
	if len(stdout) > 1000 {
		t.Fatalf("log size %d exceeded capped expectation", len(stdout))
	}
}

func TestDefaultTimeoutKills(t *testing.T) {
	cfg := DefaultConfig("")
	cfg.DefaultProcessTimeout = 100 * time.Millisecond
	client := setupTestServerWithConfig(t, cfg)

	started := sh(t, client, "sleep 30")
	proc := wait(t, client, started.GetProcessId())
	if proc.GetExitCode() != 137 {
		t.Fatalf("expected 137 (128+SIGKILL) from watchdog, got %d", proc.GetExitCode())
	}
}

func TestPerProcessTimeout(t *testing.T) {
	client := setupTestServer(t)
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sleep", "30"},
		Timeout: durationpb.New(100 * time.Millisecond),
	})
	proc := wait(t, client, started.GetProcessId())
	if proc.GetExitCode() != 137 {
		t.Fatalf("expected 137 (128+SIGKILL) from per-process timeout, got %d", proc.GetExitCode())
	}
}

func TestSignalRoundTrip(t *testing.T) {
	for api, sys := range signalTable {
		if got := FromSyscallSignal(sys); got != api {
			t.Errorf("FromSyscallSignal(%v) = %v, want %v", sys, got, api)
		}
	}
	if _, ok := ToSyscallSignal(ateenvv1alpha.Signal_SIGNAL_UNSPECIFIED); ok {
		t.Errorf("UNSPECIFIED must not map to a signal")
	}
}
