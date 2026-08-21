package process

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func setupTestServer(t *testing.T) (ateenvv1.ProcessServiceClient, func()) {
	t.Helper()
	return setupTestServerWithConfig(t, DefaultConfig(t.TempDir()))
}

func setupTestServerWithConfig(t *testing.T, cfg TrackerConfig) (ateenvv1.ProcessServiceClient, func()) {
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
	svc := NewService(tracker)
	ateenvv1.RegisterProcessServiceServer(server, svc)

	go func() {
		_ = server.Serve(lis)
	}()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}

	client := ateenvv1.NewProcessServiceClient(conn)

	cleanup := func() {
		tracker.Close()
		conn.Close()
		server.Stop()
		lis.Close()
		_ = os.RemoveAll(cfg.LogDir)
	}

	return client, cleanup
}

func TestStartAndGetProcess(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "echo 'hello from substrate'"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	if startRes.ProcessId == "" {
		t.Fatalf("expected non-empty process_id")
	}

	// Poll until completed
	var proc *ateenvv1.Process
	for i := 0; i < 20; i++ {
		proc, err = client.GetProcess(ctx, &ateenvv1.GetProcessRequest{
			ProcessId: startRes.ProcessId,
		})
		if err != nil {
			t.Fatalf("GetProcess failed: %v", err)
		}
		if proc.Status == ateenvv1.ProcessStatus_PROCESS_STATUS_COMPLETED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_COMPLETED {
		t.Fatalf("expected status COMPLETED, got %v", proc.Status)
	}
	if proc.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", proc.ExitCode)
	}
	if proc.StartedAt == nil || proc.FinishedAt == nil {
		t.Fatalf("expected non-nil timestamps")
	}
}

func TestProcessFailureExitCode(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	var proc *ateenvv1.Process
	for i := 0; i < 20; i++ {
		proc, err = client.GetProcess(ctx, &ateenvv1.GetProcessRequest{
			ProcessId: startRes.ProcessId,
		})
		if err != nil {
			t.Fatalf("GetProcess failed: %v", err)
		}
		if proc.Status == ateenvv1.ProcessStatus_PROCESS_STATUS_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_FAILED {
		t.Fatalf("expected status FAILED, got %v", proc.Status)
	}
	if proc.ExitCode != 42 {
		t.Fatalf("expected exit_code 42, got %d", proc.ExitCode)
	}
}

func TestStreamProcessLogs(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "echo 'out1'; echo 'err1' >&2; sleep 0.1; echo 'out2'"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	stream, err := client.StreamProcessLogs(ctx, &ateenvv1.StreamProcessLogsRequest{
		ProcessId: startRes.ProcessId,
		Follow:    true,
	})
	if err != nil {
		t.Fatalf("StreamProcessLogs failed: %v", err)
	}

	var stdoutBuilder strings.Builder
	var stderrBuilder strings.Builder

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("error reading log chunk: %v", err)
		}
		if chunk.Source == ateenvv1.LogSource_LOG_SOURCE_STDOUT {
			stdoutBuilder.Write(chunk.Data)
		} else if chunk.Source == ateenvv1.LogSource_LOG_SOURCE_STDERR {
			stderrBuilder.Write(chunk.Data)
		}
	}

	stdout := stdoutBuilder.String()
	stderr := stderrBuilder.String()

	if !strings.Contains(stdout, "out1") || !strings.Contains(stdout, "out2") {
		t.Fatalf("expected stdout to contain out1 and out2, got %q", stdout)
	}
	if !strings.Contains(stderr, "err1") {
		t.Fatalf("expected stderr to contain err1, got %q", stderr)
	}
}

func TestKillProcess(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	killRes, err := client.KillProcess(ctx, &ateenvv1.KillProcessRequest{
		ProcessId: startRes.ProcessId,
	})
	if err != nil {
		t.Fatalf("KillProcess failed: %v", err)
	}

	if killRes.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code after kill, got %d", killRes.ExitCode)
	}

	proc, err := client.GetProcess(ctx, &ateenvv1.GetProcessRequest{
		ProcessId: startRes.ProcessId,
	})
	if err != nil {
		t.Fatalf("GetProcess failed: %v", err)
	}

	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_TERMINATED {
		t.Fatalf("expected status TERMINATED, got %v", proc.Status)
	}
}

func TestConcurrencyLimiter(t *testing.T) {
	// Configure tracker with max 2 concurrent jobs
	cfg := DefaultConfig("")
	cfg.MaxConcurrentProcesses = 2

	client, cleanup := setupTestServerWithConfig(t, cfg)
	defer cleanup()

	ctx := context.Background()

	// Launch job 1 (running for 5s)
	res1, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatalf("job 1 failed: %v", err)
	}

	// Launch job 2 (running for 5s)
	res2, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatalf("job 2 failed: %v", err)
	}

	// Launch job 3 -> Must be rejected with ResourceExhausted!
	_, err = client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "echo 'should fail'"},
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted for job 3, got: %v", err)
	}

	// Kill job 1 to free up a slot
	_, err = client.KillProcess(ctx, &ateenvv1.KillProcessRequest{
		ProcessId: res1.ProcessId,
	})
	if err != nil {
		t.Fatalf("failed to kill job 1: %v", err)
	}

	// Now job 3 should succeed
	res3, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "echo 'now succeeds'"},
	})
	if err != nil {
		t.Fatalf("job 3 failed after slot freed: %v", err)
	}
	if res3.ProcessId == "" {
		t.Fatalf("expected non-empty process ID for job 3")
	}

	// Clean up job 2
	_, _ = client.KillProcess(ctx, &ateenvv1.KillProcessRequest{ProcessId: res2.ProcessId})
}

func TestLogCapping(t *testing.T) {
	// Configure max log size to 256 bytes
	cfg := DefaultConfig("")
	cfg.MaxLogBytes = 256

	client, cleanup := setupTestServerWithConfig(t, cfg)
	defer cleanup()

	ctx := context.Background()

	// Command outputs 10,000 bytes of spam
	res, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "for i in $(seq 1 500); do echo 'spamming-log-line-0123456789'; done"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	stream, err := client.StreamProcessLogs(ctx, &ateenvv1.StreamProcessLogsRequest{
		ProcessId: res.ProcessId,
		Follow:    true,
	})
	if err != nil {
		t.Fatalf("StreamProcessLogs failed: %v", err)
	}

	var stdout strings.Builder
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream recv error: %v", err)
		}
		if chunk.Source == ateenvv1.LogSource_LOG_SOURCE_STDOUT {
			stdout.Write(chunk.Data)
		}
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "maximum log limit") || !strings.Contains(outStr, "truncated") {
		t.Fatalf("expected truncation warning in capped logs, got:\n%s", outStr)
	}

	// Total length should be close to 256 bytes + truncation banner (~350 bytes), not 15,000 bytes!
	if len(outStr) > 1000 {
		t.Fatalf("log size %d exceeded capped expectation", len(outStr))
	}
}

func TestWatchdogTimeout(t *testing.T) {
	// Configure 100ms watchdog timeout
	cfg := DefaultConfig("")
	cfg.DefaultProcessTimeout = 100 * time.Millisecond

	client, cleanup := setupTestServerWithConfig(t, cfg)
	defer cleanup()

	ctx := context.Background()

	// Process attempts to sleep 30 seconds
	res, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	// Wait 250ms for watchdog timer to trigger
	time.Sleep(250 * time.Millisecond)

	proc, err := client.GetProcess(ctx, &ateenvv1.GetProcessRequest{
		ProcessId: res.ProcessId,
	})
	if err != nil {
		t.Fatalf("GetProcess failed: %v", err)
	}

	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_TERMINATED {
		t.Fatalf("expected status TERMINATED by watchdog timeout, got %v", proc.Status)
	}
	if proc.ExitCode != 137 {
		t.Fatalf("expected exit code 137, got %d", proc.ExitCode)
	}
}
