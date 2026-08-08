package shell_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	guestsys "github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/tool"
	"github.com/agent-substrate/env/internal/tool/shell"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestShellTool(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sys := guestsys.New()

	reg := tool.NewRegistry()
	if err := reg.Register(shell.New(sys, shell.Config{})); err != nil {
		t.Fatalf("Register shell tool: %v", err)
	}

	rawInput, _ := json.Marshal(map[string]any{
		"command": "echo hello_shell",
	})
	res := reg.Invoke(context.Background(), "shell", rawInput)

	if res.IsError {
		t.Fatalf("shell tool invocation failed: %v", res)
	}

	if len(res.Content) == 0 {
		t.Fatalf("unexpected empty shell output")
	}
	txt, ok := res.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(txt.Text, "hello_shell") {
		t.Errorf("unexpected shell output: %+v", res.Content[0])
	}
}
