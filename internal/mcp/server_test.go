package mcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/ate"
	internalmcp "github.com/agent-substrate/env/internal/mcp"
	"github.com/agent-substrate/env/internal/tool"
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
	req := httptest.NewRequest("POST", "/v1/envs//mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound && rec.Code != http.StatusInternalServerError {
		t.Errorf("unexpected status code: %d", rec.Code)
	}
}
