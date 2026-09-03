package mcp_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-substrate/env/guest"
	internalmcp "github.com/agent-substrate/env/internal/mcp"
	"github.com/agent-substrate/env/internal/tool"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func setupTestServer(t *testing.T) (ateenvv1alpha.FileSystemServiceClient, ateenvv1alpha.ProcessServiceClient, func()) {
	t.Helper()

	tempDir := t.TempDir()
	logDir := filepath.Join(tempDir, "logs")
	workspace := filepath.Join(tempDir, "workspace")
	_ = os.MkdirAll(logDir, 0755)
	_ = os.MkdirAll(workspace, 0755)

	cfg := guest.Config{
		ListenAddr:       ":0",
		LogDir:           logDir,
		Workspace:        workspace,
		EnableProcess:    true,
		EnableFileSystem: true,
	}

	grpcServer, cleanup, err := guest.NewServer(cfg)
	if err != nil {
		t.Fatalf("failed to create guest server: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial server: %v", err)
	}

	fsClient := ateenvv1alpha.NewFileSystemServiceClient(conn)
	procClient := ateenvv1alpha.NewProcessServiceClient(conn)

	teardown := func() {
		conn.Close()
		grpcServer.GracefulStop()
		lis.Close()
		cleanup()
	}

	return fsClient, procClient, teardown
}

func TestMCPFileSystemTools(t *testing.T) {
	fsClient, _, teardown := setupTestServer(t)
	defer teardown()

	tools := internalmcp.NewFileSystemTools(fsClient)
	reg := tool.NewRegistry()
	if err := reg.Register(tools...); err != nil {
		t.Fatalf("registering filesystem tools: %v", err)
	}

	ctx := context.Background()

	// 1. Test write_file
	writeInput, _ := json.Marshal(map[string]any{
		"path":    "hello.txt",
		"content": "Hello World gRPC MCP!",
	})
	res := reg.Invoke(ctx, "write_file", writeInput)
	if res.IsError {
		t.Fatalf("write_file failed: %v", res)
	}

	// 2. Test read_file
	readInput, _ := json.Marshal(map[string]any{
		"path": "hello.txt",
	})
	res = reg.Invoke(ctx, "read_file", readInput)
	if res.IsError {
		t.Fatalf("read_file failed: %v", res)
	}
	if len(res.Content) == 0 {
		t.Fatalf("expected content, got empty")
	}
	txt, ok := res.Content[0].(*mcp.TextContent)
	if !ok || txt.Text != "Hello World gRPC MCP!" {
		t.Fatalf("unexpected content: %v", res.Content[0])
	}
}

func TestMCPProcessTools(t *testing.T) {
	_, procClient, teardown := setupTestServer(t)
	defer teardown()

	tools := internalmcp.NewProcessTools(procClient)
	reg := tool.NewRegistry()
	if err := reg.Register(tools...); err != nil {
		t.Fatalf("registering process tools: %v", err)
	}

	ctx := context.Background()

	// 1. Test shell
	shellInput, _ := json.Marshal(map[string]any{
		"command": "echo 'hello from shell'",
	})
	res := reg.Invoke(ctx, "shell", shellInput)
	if res.IsError {
		t.Fatalf("shell failed: %v", res)
	}
	shellOut := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(shellOut, "hello from shell") {
		t.Fatalf("unexpected shell output: %q", shellOut)
	}
}

func TestMCPAllTools(t *testing.T) {
	fsClient, procClient, teardown := setupTestServer(t)
	defer teardown()

	tools := internalmcp.NewTools(fsClient, procClient)
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}

	reg := tool.NewRegistry()
	if err := reg.Register(tools...); err != nil {
		t.Fatalf("registering all tools: %v", err)
	}

	names := reg.Names()
	expected := []string{"read_file", "shell", "write_file"}
	if len(names) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, names)
	}
	for i, name := range expected {
		if names[i] != name {
			t.Fatalf("at index %d expected %s, got %s", i, name, names[i])
		}
	}
}
