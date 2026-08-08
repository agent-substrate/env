package fs_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	guestsys "github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/tool"
	fstool "github.com/agent-substrate/env/internal/tool/fs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func invokeTool(t *testing.T, reg *tool.Registry, toolName string, input map[string]any) *mcp.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return reg.Invoke(context.Background(), toolName, raw)
}

func getText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func TestFSTools(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sys := guestsys.New()

	reg := tool.NewRegistry()
	if err := reg.Register(fstool.New(sys, fstool.Config{})...); err != nil {
		t.Fatalf("Register fs tools: %v", err)
	}

	// 1. write_file
	res := invokeTool(t, reg, "write_file", map[string]any{
		"path":    "hello.txt",
		"content": "Line 1: Hello World\nLine 2: Substrate Tools\n",
	})
	if res.IsError {
		t.Fatalf("write_file error: %v", res)
	}

	// 2. read_file
	res = invokeTool(t, reg, "read_file", map[string]any{
		"path": "hello.txt",
	})
	if res.IsError || !strings.Contains(getText(res), "Hello World") {
		t.Fatalf("read_file failed: %v", res)
	}

	// 3. stat
	res = invokeTool(t, reg, "stat", map[string]any{
		"path": "hello.txt",
	})
	if res.IsError || !strings.Contains(getText(res), "type: file") {
		t.Fatalf("stat failed: %v", res)
	}

	// 4. edit_file
	res = invokeTool(t, reg, "edit_file", map[string]any{
		"path":       "hello.txt",
		"old_string": "Substrate Tools",
		"new_string": "Substrate Environment",
	})
	if res.IsError {
		t.Fatalf("edit_file failed: %v", res)
	}

	res = invokeTool(t, reg, "read_file", map[string]any{
		"path": "hello.txt",
	})
	if !strings.Contains(getText(res), "Substrate Environment") {
		t.Fatalf("read_file after edit failed: %v", res)
	}

	// 5. mkdir
	res = invokeTool(t, reg, "mkdir", map[string]any{
		"path": "nested/dir",
	})
	if res.IsError {
		t.Fatalf("mkdir failed: %v", res)
	}

	// 6. mv
	res = invokeTool(t, reg, "mv", map[string]any{
		"source":      "hello.txt",
		"destination": "nested/dir/moved.txt",
	})
	if res.IsError {
		t.Fatalf("mv failed: %v", res)
	}

	// 7. list_dir
	res = invokeTool(t, reg, "list_dir", map[string]any{
		"path": "nested/dir",
	})
	if res.IsError || !strings.Contains(getText(res), "moved.txt") {
		t.Fatalf("list_dir failed: %v", res)
	}

	// 8. glob
	res = invokeTool(t, reg, "glob", map[string]any{
		"pattern": "**/*.txt",
	})
	if res.IsError || !strings.Contains(getText(res), "moved.txt") {
		t.Fatalf("glob failed: %v", res)
	}

	// 9. grep
	res = invokeTool(t, reg, "grep", map[string]any{
		"pattern": "Substrate",
	})
	if res.IsError || !strings.Contains(getText(res), "Substrate Environment") {
		t.Fatalf("grep failed: %v", res)
	}

	// 10. rm
	res = invokeTool(t, reg, "rm", map[string]any{
		"path":      "nested",
		"recursive": true,
	})
	if res.IsError {
		t.Fatalf("rm failed: %v", res)
	}

	if _, err := os.Stat(filepath.Join(dir, "nested")); !os.IsNotExist(err) {
		t.Fatalf("nested dir still exists after rm: %v", err)
	}
}
