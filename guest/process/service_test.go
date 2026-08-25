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

func TestStreamProcessLogsWithOffset(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "echo 'prefix-to-skip'; echo 'streamed-line'"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	skipLen := int64(len("prefix-to-skip\n"))
	stream, err := client.StreamProcessLogs(ctx, &ateenvv1.StreamProcessLogsRequest{
		ProcessId:    startRes.ProcessId,
		StdoutOffset: skipLen,
		Follow:       false,
	})
	if err != nil {
		t.Fatalf("StreamProcessLogs failed: %v", err)
	}

	var stdoutBuilder strings.Builder
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
		}
	}

	out := stdoutBuilder.String()
	if strings.Contains(out, "prefix-to-skip") {
		t.Fatalf("expected prefix-to-skip to be skipped, got %q", out)
	}
	if !strings.Contains(out, "streamed-line") {
		t.Fatalf("expected streamed-line in output, got %q", out)
	}
}

func TestStreamProcessLogsSnapshotNoFollow(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "echo 'instant-output'; sleep 5"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	stream, err := client.StreamProcessLogs(ctx, &ateenvv1.StreamProcessLogsRequest{
		ProcessId: startRes.ProcessId,
		Follow:    false,
	})
	if err != nil {
		t.Fatalf("StreamProcessLogs failed: %v", err)
	}

	var stdoutBuilder strings.Builder
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("error reading chunk: %v", err)
		}
		stdoutBuilder.Write(chunk.Data)
	}
	duration := time.Since(start)

	if duration > 2*time.Second {
		t.Fatalf("snapshot mode (follow=false) took %v, should have returned immediately", duration)
	}
	if !strings.Contains(stdoutBuilder.String(), "instant-output") {
		t.Fatalf("expected output in snapshot, got %q", stdoutBuilder.String())
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

	if killRes.ExitCode != 137 {
		t.Fatalf("expected exit code 137 after kill, got %d", killRes.ExitCode)
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
	if proc.ExitCode != 137 {
		t.Fatalf("expected exit code 137, got %d", proc.ExitCode)
	}
}

func TestProcessSignalDeath(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "kill -15 $$"},
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
		if proc.Status == ateenvv1.ProcessStatus_PROCESS_STATUS_TERMINATED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_TERMINATED {
		t.Fatalf("expected status TERMINATED, got %v", proc.Status)
	}
	if proc.ExitCode != 143 { // 128 + 15 (SIGTERM)
		t.Fatalf("expected exit code 143 (128+SIGTERM), got %d", proc.ExitCode)
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

// collectSnapshot performs one non-follow StreamProcessLogs call and returns
// the received stdout and stderr bytes plus the size of the largest single
// chunk seen on the stream.
func collectSnapshot(ctx context.Context, t *testing.T, client ateenvv1.ProcessServiceClient, processID string, stdoutOffset, stderrOffset, waitMs int64) (stdout, stderr []byte, maxChunk int) {
	t.Helper()

	stream, err := client.StreamProcessLogs(ctx, &ateenvv1.StreamProcessLogsRequest{
		ProcessId:    processID,
		StdoutOffset: stdoutOffset,
		StderrOffset: stderrOffset,
		Follow:       false,
		WaitMs:       waitMs,
	})
	if err != nil {
		t.Fatalf("StreamProcessLogs failed: %v", err)
	}

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return stdout, stderr, maxChunk
		}
		if err != nil {
			t.Fatalf("snapshot recv error: %v", err)
		}
		if len(chunk.Data) > maxChunk {
			maxChunk = len(chunk.Data)
		}
		switch chunk.Source {
		case ateenvv1.LogSource_LOG_SOURCE_STDOUT:
			stdout = append(stdout, chunk.Data...)
		case ateenvv1.LogSource_LOG_SOURCE_STDERR:
			stderr = append(stderr, chunk.Data...)
		}
	}
}

// waitForProcessDone polls GetProcess until the process leaves RUNNING or the
// timeout elapses, returning the final Process resource.
func waitForProcessDone(ctx context.Context, t *testing.T, client ateenvv1.ProcessServiceClient, processID string, timeout time.Duration) *ateenvv1.Process {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		proc, err := client.GetProcess(ctx, &ateenvv1.GetProcessRequest{ProcessId: processID})
		if err != nil {
			t.Fatalf("GetProcess failed: %v", err)
		}
		if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING {
			return proc
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %s still RUNNING after %v", processID, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWriteStdinEcho(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"cat"},
		Stdin:   true,
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	payload := "echoed-through-stdin\n"
	if _, err := client.WriteStdin(ctx, &ateenvv1.WriteStdinRequest{
		ProcessId: startRes.ProcessId,
		Data:      []byte(payload),
	}); err != nil {
		t.Fatalf("WriteStdin failed: %v", err)
	}

	// Poll bounded snapshots until cat echoes the payload back on stdout.
	var stdout []byte
	var stdoutOffset int64
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(string(stdout), payload) {
		if time.Now().After(deadline) {
			t.Fatalf("stdin payload never echoed on stdout, got %q", stdout)
		}
		out, _, _ := collectSnapshot(ctx, t, client, startRes.ProcessId, stdoutOffset, 0, 1000)
		stdout = append(stdout, out...)
		stdoutOffset += int64(len(out))
	}

	// Close stdin: cat reads EOF and exits cleanly.
	if _, err := client.WriteStdin(ctx, &ateenvv1.WriteStdinRequest{
		ProcessId: startRes.ProcessId,
		Close:     true,
	}); err != nil {
		t.Fatalf("WriteStdin close failed: %v", err)
	}

	proc := waitForProcessDone(ctx, t, client, startRes.ProcessId, 30*time.Second)
	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_COMPLETED {
		t.Fatalf("expected status COMPLETED after stdin close, got %v", proc.Status)
	}
	if proc.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", proc.ExitCode)
	}
}

func TestStartProcessStdinDefaultClosed(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	// Without stdin, cat must read EOF immediately instead of blocking forever.
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "cat; echo done"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	proc := waitForProcessDone(ctx, t, client, startRes.ProcessId, 30*time.Second)
	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_COMPLETED {
		t.Fatalf("expected status COMPLETED with stdin off, got %v", proc.Status)
	}
	if proc.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", proc.ExitCode)
	}

	stdout, _, _ := collectSnapshot(ctx, t, client, startRes.ProcessId, 0, 0, 0)
	if !strings.Contains(string(stdout), "done") {
		t.Fatalf("expected 'done' on stdout, got %q", stdout)
	}

	// Writing to a process started without stdin is a failed precondition.
	_, err = client.WriteStdin(ctx, &ateenvv1.WriteStdinRequest{
		ProcessId: startRes.ProcessId,
		Data:      []byte("too late"),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition writing to stdin-less process, got %v", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "stdin") {
		t.Fatalf("expected error message to mention stdin, got %q", status.Convert(err).Message())
	}
}

func TestWriteStdinValidation(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.WriteStdin(ctx, &ateenvv1.WriteStdinRequest{
		ProcessId: "proc-does-not-exist",
		Data:      []byte("x"),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for unknown process, got %v", err)
	}

	_, err = client.WriteStdin(ctx, &ateenvv1.WriteStdinRequest{
		ProcessId: "",
		Data:      []byte("x"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty process_id, got %v", err)
	}
}

func TestKillProcessSigterm(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// SIGTERM is delivered and returns promptly; the outcome is polled.
	if _, err := client.KillProcess(ctx, &ateenvv1.KillProcessRequest{
		ProcessId: startRes.ProcessId,
		Signal:    15,
	}); err != nil {
		t.Fatalf("KillProcess(SIGTERM) failed: %v", err)
	}

	proc := waitForProcessDone(ctx, t, client, startRes.ProcessId, 30*time.Second)
	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_TERMINATED {
		t.Fatalf("expected status TERMINATED after SIGTERM, got %v", proc.Status)
	}
	if proc.ExitCode != 143 { // 128 + 15 (SIGTERM)
		t.Fatalf("expected exit code 143 (128+SIGTERM), got %d", proc.ExitCode)
	}
}

func TestKillProcessDefaultSignalIsSigkill(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Signal 0 preserves KillProcess's original SIGKILL behavior.
	killRes, err := client.KillProcess(ctx, &ateenvv1.KillProcessRequest{
		ProcessId: startRes.ProcessId,
	})
	if err != nil {
		t.Fatalf("KillProcess failed: %v", err)
	}
	if killRes.ExitCode != 137 { // 128 + 9 (SIGKILL)
		t.Fatalf("expected exit code 137 after default kill, got %d", killRes.ExitCode)
	}

	proc := waitForProcessDone(ctx, t, client, startRes.ProcessId, 30*time.Second)
	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_TERMINATED {
		t.Fatalf("expected status TERMINATED, got %v", proc.Status)
	}
	if proc.ExitCode != 137 {
		t.Fatalf("expected exit code 137, got %d", proc.ExitCode)
	}
}

func TestStreamProcessLogsWaitMsLongPoll(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "sleep 0.3; echo tick"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	// Nothing is on stdout yet: the long-poll must hold the request until
	// "tick" lands and return with the data rather than an empty snapshot.
	stdout, _, _ := collectSnapshot(ctx, t, client, startRes.ProcessId, 0, 0, 5000)
	if !strings.Contains(string(stdout), "tick") {
		t.Fatalf("expected long-poll snapshot to return tick, got %q", stdout)
	}

	// A second long-poll from the advanced offset has nothing left to see;
	// it must still complete (bounded by process exit / wait_ms) with no
	// data instead of hanging.
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout2, stderr2, _ := collectSnapshot(ctx2, t, client, startRes.ProcessId, int64(len(stdout)), 0, 5000)
	if len(stdout2) != 0 || len(stderr2) != 0 {
		t.Fatalf("expected empty second snapshot, got stdout=%q stderr=%q", stdout2, stderr2)
	}
}

func TestStreamProcessLogsChunking(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	const totalBytes = 3000000
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "head -c 3000000 /dev/zero | tr '\\0' 'x'"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	// Poll snapshots with advancing offsets until the process is done and a
	// post-exit drain has been performed (the reaper syncs the log files
	// before flipping the status, so that drain is complete).
	var stdoutOffset int64
	maxChunkSeen := 0
	deadline := time.Now().Add(30 * time.Second)
	var proc *ateenvv1.Process
	for {
		proc, err = client.GetProcess(ctx, &ateenvv1.GetProcessRequest{ProcessId: startRes.ProcessId})
		if err != nil {
			t.Fatalf("GetProcess failed: %v", err)
		}
		done := proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING

		out, _, maxChunk := collectSnapshot(ctx, t, client, startRes.ProcessId, stdoutOffset, 0, 1000)
		stdoutOffset += int64(len(out))
		if maxChunk > maxChunkSeen {
			maxChunkSeen = maxChunk
		}

		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process still RUNNING after 30s; received %d bytes so far", stdoutOffset)
		}
	}

	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_COMPLETED {
		t.Fatalf("expected status COMPLETED, got %v", proc.Status)
	}
	if stdoutOffset != totalBytes {
		t.Fatalf("expected %d accumulated stdout bytes, got %d", totalBytes, stdoutOffset)
	}
	if maxChunkSeen > 1<<20 {
		t.Fatalf("received a %d-byte chunk, exceeding the 1 MiB chunk cap", maxChunkSeen)
	}
}

func TestStreamProcessLogsOffsetChaining(t *testing.T) {
	client, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	startRes, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: []string{"sh", "-c", "printf out-a; printf err-a >&2; sleep 0.2; printf out-b; printf err-b >&2; sleep 0.2; printf out-c"},
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	// Chain snapshots: each call resumes from the previous offset plus the
	// bytes received per source. Exact final content proves no output was
	// duplicated or skipped across the chained calls.
	var stdoutOffset, stderrOffset int64
	var stdoutAll, stderrAll []byte
	deadline := time.Now().Add(30 * time.Second)
	var proc *ateenvv1.Process
	for {
		proc, err = client.GetProcess(ctx, &ateenvv1.GetProcessRequest{ProcessId: startRes.ProcessId})
		if err != nil {
			t.Fatalf("GetProcess failed: %v", err)
		}
		done := proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING

		out, errOut, _ := collectSnapshot(ctx, t, client, startRes.ProcessId, stdoutOffset, stderrOffset, 500)
		stdoutAll = append(stdoutAll, out...)
		stderrAll = append(stderrAll, errOut...)
		stdoutOffset += int64(len(out))
		stderrOffset += int64(len(errOut))

		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process still RUNNING after 30s; stdout=%q stderr=%q", stdoutAll, stderrAll)
		}
	}

	if proc.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_COMPLETED {
		t.Fatalf("expected status COMPLETED, got %v", proc.Status)
	}

	wantStdout := "out-aout-bout-c"
	wantStderr := "err-aerr-b"
	if string(stdoutAll) != wantStdout {
		t.Fatalf("chained stdout mismatch: expected %q, got %q", wantStdout, stdoutAll)
	}
	if string(stderrAll) != wantStderr {
		t.Fatalf("chained stderr mismatch: expected %q, got %q", wantStderr, stderrAll)
	}
	if stdoutOffset != int64(len(wantStdout)) {
		t.Fatalf("expected final stdout offset %d, got %d", len(wantStdout), stdoutOffset)
	}
	if stderrOffset != int64(len(wantStderr)) {
		t.Fatalf("expected final stderr offset %d, got %d", len(wantStderr), stderrOffset)
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
