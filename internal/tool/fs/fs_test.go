package fs_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	guestsys "github.com/agent-substrate/sandbox/internal/guest/guestsys"
	"github.com/agent-substrate/sandbox/internal/tool"
	fstool "github.com/agent-substrate/sandbox/internal/tool/fs"
)

func invokeTool(t *testing.T, reg *tool.Registry, toolName, callID string, input map[string]any) tool.ToolResult {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return reg.Invoke(context.Background(), tool.ToolUse{
		Type:  tool.BlockTypeToolUse,
		ID:    callID,
		Name:  toolName,
		Input: raw,
	})
}

func TestFSTools(t *testing.T) {
	dir := t.TempDir()
	fsSys, err := guestsys.New(dir)
	if err != nil {
		t.Fatalf("guestsys.New: %v", err)
	}

	reg := tool.NewRegistry()
	if err := reg.Register(fstool.New(fsSys, fstool.Config{})...); err != nil {
		t.Fatalf("Register fs tools: %v", err)
	}

	// 1. write_file
	res := invokeTool(t, reg, "write_file", "call_write", map[string]any{
		"path":    "hello.txt",
		"content": "Line 1: Hello World\nLine 2: Substrate Tools\n",
	})
	if res.IsError {
		t.Fatalf("write_file error: %v", res)
	}

	// 2. read_file
	res = invokeTool(t, reg, "read_file", "call_read", map[string]any{
		"path": "hello.txt",
	})
	if res.IsError || !strings.Contains(res.Content[0].Text, "Hello World") {
		t.Fatalf("read_file failed: %v", res)
	}

	// 3. stat
	res = invokeTool(t, reg, "stat", "call_stat", map[string]any{
		"path": "hello.txt",
	})
	if res.IsError || !strings.Contains(res.Content[0].Text, "type: file") {
		t.Fatalf("stat failed: %v", res)
	}

	// 4. edit_file
	res = invokeTool(t, reg, "edit_file", "call_edit", map[string]any{
		"path":       "hello.txt",
		"old_string": "Hello World",
		"new_string": "Hello Substrate",
	})
	if res.IsError {
		t.Fatalf("edit_file error: %v", res)
	}

	// 5. grep
	res = invokeTool(t, reg, "grep", "call_grep", map[string]any{
		"pattern": "Hello Substrate",
	})
	if res.IsError || !strings.Contains(res.Content[0].Text, "hello.txt:1:") {
		t.Fatalf("grep failed: %v", res)
	}

	// 6. list_dir
	res = invokeTool(t, reg, "list_dir", "call_list", map[string]any{
		"path": ".",
	})
	if res.IsError || !strings.Contains(res.Content[0].Text, "hello.txt") {
		t.Fatalf("list_dir failed: %v", res)
	}

	// 7. glob
	res = invokeTool(t, reg, "glob", "call_glob", map[string]any{
		"pattern": "*.txt",
	})
	if res.IsError || !strings.Contains(res.Content[0].Text, "hello.txt") {
		t.Fatalf("glob failed: %v", res)
	}

	// 8. mkdir
	res = invokeTool(t, reg, "mkdir", "call_mkdir", map[string]any{
		"path": "subdir",
	})
	if res.IsError {
		t.Fatalf("mkdir error: %v", res)
	}

	// 9. mv
	res = invokeTool(t, reg, "mv", "call_mv", map[string]any{
		"source":      "hello.txt",
		"destination": "subdir/hello_moved.txt",
	})
	if res.IsError {
		t.Fatalf("mv error: %v", res)
	}

	// 10. rm
	res = invokeTool(t, reg, "rm", "call_rm", map[string]any{
		"path":      "subdir",
		"recursive": true,
	})
	if res.IsError {
		t.Fatalf("rm error: %v", res)
	}

	if _, err := os.Stat(filepath.Join(dir, "subdir")); !os.IsNotExist(err) {
		t.Errorf("subdir still exists after recursive rm")
	}
}
