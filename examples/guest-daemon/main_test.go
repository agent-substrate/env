package main

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-substrate/env/guest"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestGuestDaemonIntegration(t *testing.T) {
	tempDir := t.TempDir()
	logDir := filepath.Join(tempDir, "logs")

	cfg := guest.Config{
		LogDir:           logDir,
		Workspace:        tempDir,
		EnableProcess:    true,
		EnableFileSystem: true,
	}

	grpcServer, cleanup, err := guest.NewServer(cfg)
	if err != nil {
		t.Fatalf("failed to initialize guest daemon: %v", err)
	}
	defer cleanup()

	lis := bufconn.Listen(1024 * 1024)
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.Stop()

	ctx := context.Background()
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}
	defer conn.Close()

	procClient := ateenvv1alpha.NewProcessServiceClient(conn)
	fsClient := ateenvv1alpha.NewFileSystemServiceClient(conn)

	// 1. Write a Python script to disk using FileSystemService
	scriptPath := filepath.Join(tempDir, "test_job.py")
	scriptContent := []byte(`
import sys
print("Substrate Python Job: Success")
sys.stderr.write("Job stderr log\n")
`)

	writeStream, err := fsClient.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	err = writeStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  scriptPath,
		Chunk: scriptContent,
		Mode:  0755,
	})
	if err != nil {
		t.Fatalf("failed to send chunk: %v", err)
	}
	writeRes, err := writeStream.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteFile CloseAndRecv failed: %v", err)
	}
	if writeRes.BytesWritten != int64(len(scriptContent)) {
		t.Fatalf("expected %d bytes written, got %d", len(scriptContent), writeRes.BytesWritten)
	}

	// 2. Start execution using ProcessService
	startRes, err := procClient.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"python3", scriptPath},
		Cwd:     tempDir,
	})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	// 3. Stream real-time output
	outStream, err := procClient.StreamProcessOutputs(ctx, &ateenvv1alpha.StreamProcessOutputsRequest{
		ProcessId: startRes.ProcessId,
		Follow:    true,
	})
	if err != nil {
		t.Fatalf("StreamProcessOutputs failed: %v", err)
	}

	var stdout strings.Builder
	var stderr strings.Builder
	for {
		chunk, err := outStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("error reading output chunk: %v", err)
		}
		if chunk.Source == ateenvv1alpha.OutputSource_OUTPUT_SOURCE_STDOUT {
			stdout.Write(chunk.Data)
		} else if chunk.Source == ateenvv1alpha.OutputSource_OUTPUT_SOURCE_STDERR {
			stderr.Write(chunk.Data)
		}
	}

	if !strings.Contains(stdout.String(), "Substrate Python Job: Success") {
		t.Fatalf("expected stdout to contain job success, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Job stderr log") {
		t.Fatalf("expected stderr to contain 'Job stderr log', got %q", stderr.String())
	}

	// 4. Verify Process metadata
	proc, err := procClient.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{
		ProcessId: startRes.ProcessId,
	})
	if err != nil {
		t.Fatalf("GetProcess failed: %v", err)
	}
	if proc.Status != ateenvv1alpha.ProcessStatus_PROCESS_STATUS_COMPLETED {
		t.Fatalf("expected status COMPLETED, got %v", proc.Status)
	}
	if proc.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", proc.ExitCode)
	}
}
