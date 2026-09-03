package mcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	guestdaemon "github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	internalmcp "github.com/agent-substrate/env/internal/mcp"
	"github.com/agent-substrate/env/internal/tool"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPServer(t *testing.T) {
	fsClient, procClient, teardown := setupTestServer(t)
	defer teardown()

	mcpSrv := internalmcp.NewServerForClients(fsClient, procClient)
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)

	t1, t2 := mcp.NewInMemoryTransports()
	ctx := context.Background()

	sSession, err := mcpSrv.MCPServer().Connect(ctx, t1, nil)
	if err != nil {
		t.Fatalf("Connect server: %v", err)
	}
	defer sSession.Close()

	cSession, err := c.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("Connect client: %v", err)
	}
	defer cSession.Close()

	tools, err := cSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatalf("expected tools in ListTools result")
	}

	callRes, err := cSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "shell",
		Arguments: map[string]any{"command": "echo hello_mcp_sdk"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if callRes.IsError || len(callRes.Content) == 0 {
		t.Fatalf("CallTool failed: %+v", callRes)
	}
	txt, ok := callRes.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(txt.Text, "hello_mcp_sdk") {
		t.Errorf("unexpected content: %+v", callRes.Content)
	}
}

func TestMCPServerNewServer(t *testing.T) {
	reg := tool.NewRegistry()
	srv := internalmcp.NewServer(reg)
	if srv.MCPServer() == nil {
		t.Fatalf("expected non-nil MCPServer")
	}
}

func TestMCPNewHandlerInvalidID(t *testing.T) {
	client, err := ate.New(ate.Options{
		ControlAddr: "localhost:1234",
		RouterAddr:  "localhost:5678",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	h := internalmcp.NewHandler(client)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1alpha/envs//mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound && rec.Code != http.StatusInternalServerError {
		t.Errorf("unexpected status code: %d", rec.Code)
	}
}

func TestGuestMCPProxy(t *testing.T) {
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

	mux := http.NewServeMux()
	mux.Handle("/v1alpha/envs/{id}/mcp", internalmcp.NewHandler(client))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

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
		ID:       "web-1",
		Template: "default-env",
		Atespace: "envs",
	}); err != nil {
		t.Fatalf("client.Create: %v", err)
	}

	// MCP endpoint served directly by ate-env-api communicating with guest gRPC.
	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)

	transport := &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/v1alpha/envs/web-1/mcp",
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
	if len(tools.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools.Tools))
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
