package shell_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	guestsys "github.com/agent-substrate/sandbox/internal/guest/guestsys"
	"github.com/agent-substrate/sandbox/internal/tool"
	"github.com/agent-substrate/sandbox/internal/tool/shell"
)

func TestShellTool(t *testing.T) {
	dir := t.TempDir()
	fsSys, err := guestsys.New(dir)
	if err != nil {
		t.Fatalf("guestfs.New: %v", err)
	}

	reg := tool.NewRegistry()
	if err := reg.Register(shell.New(fsSys, shell.Config{Workdir: dir})); err != nil {
		t.Fatalf("Register shell tool: %v", err)
	}

	rawInput, _ := json.Marshal(map[string]any{
		"command": "echo hello_shell",
	})
	res := reg.Invoke(context.Background(), tool.ToolUse{
		Type:  tool.BlockTypeToolUse,
		ID:    "call_sh",
		Name:  "shell",
		Input: rawInput,
	})

	if res.IsError {
		t.Fatalf("shell tool invocation failed: %v", res)
	}

	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "hello_shell") {
		t.Errorf("unexpected shell output: %v", res)
	}
}
