package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/tool"
	"github.com/agent-substrate/env/internal/tool/shell"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPServer(t *testing.T) {
	dir := t.TempDir()
	fsSys, err := guestsys.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry()
	if err := reg.Register(shell.New(fsSys, shell.Config{})); err != nil {
		t.Fatal(err)
	}

	mcpSrv := NewServer(reg)
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)

	t1, t2 := mcp.NewInMemoryTransports()
	ctx := context.Background()

	sSession, err := mcpSrv.mcpServer.Connect(ctx, t1, nil)
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
