package service_test

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	guestdaemon "github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/service"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newAPI(t *testing.T) (*httptest.Server, *fakerouter.Router, *fakecontrol.Server, *ate.Client) {
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

	srv := httptest.NewServer(service.Handler(client))
	t.Cleanup(srv.Close)
	return srv, router, control, client
}

func TestGuestMCPProxy(t *testing.T) {
	srv, router, _, client := newAPI(t)
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	logDir := filepath.Join(tempDir, "logs")
	workspace := filepath.Join(tempDir, "workspace")
	grpcGuestServer, cleanup, err := guestdaemon.NewServer(guestdaemon.Config{
		LogDir:           logDir,
		Workspace:        workspace,
		EnableFileSystem: true,
		EnableProcess:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	// Register gRPC guest server for actor web-1
	router.Register("web-1", grpcGuestServer)

	// Create environment via direct client.
	if err := client.Create(t.Context(), ate.CreateOptions{
		ID:        "web-1",
		Template:  "default-env",
		Namespace: "envs",
	}); err != nil {
		t.Fatalf("client.Create: %v", err)
	}

	// MCP endpoint served directly by ate-env-api communicating with guest gRPC.
	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)

	transport := &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/v1/envs/web-1/mcp",
	}

	ctx := t.Context()
	session, err := mcpClient.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("mcpClient.Connect: %v", err)
	}
	defer session.Close()

	// 1. List tools
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) < 7 {
		t.Fatalf("expected at least 7 tools, got %d", len(tools.Tools))
	}

	// 2. Call write_file tool over MCP
	writeRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "mcp_test.txt",
			"content": "hello mcp through ate-env-api",
		},
	})
	if err != nil {
		t.Fatalf("CallTool write_file: %v", err)
	}
	if writeRes.IsError {
		t.Fatalf("write_file returned error: %+v", writeRes)
	}

	// 3. Call read_file tool over MCP
	readRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_file",
		Arguments: map[string]any{
			"path": "mcp_test.txt",
		},
	})
	if err != nil {
		t.Fatalf("CallTool read_file: %v", err)
	}
	if readRes.IsError || len(readRes.Content) == 0 {
		t.Fatalf("read_file returned error: %+v", readRes)
	}
	readText := readRes.Content[0].(*mcp.TextContent).Text
	if readText != "hello mcp through ate-env-api" {
		t.Fatalf("read_file output = %q, want %q", readText, "hello mcp through ate-env-api")
	}

	// 4. Call shell tool over MCP
	shellRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "shell",
		Arguments: map[string]any{
			"command": "echo 'mcp shell works'",
		},
	})
	if err != nil {
		t.Fatalf("CallTool shell: %v", err)
	}
	if shellRes.IsError || len(shellRes.Content) == 0 {
		t.Fatalf("shell returned error: %+v", shellRes)
	}
	shellText := shellRes.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(shellText, "mcp shell works") {
		t.Fatalf("shell output = %q, want 'mcp shell works'", shellText)
	}
}
