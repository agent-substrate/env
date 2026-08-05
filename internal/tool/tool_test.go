package tool_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent-substrate/sandbox/internal/tool"
)

func TestRegistry(t *testing.T) {
	reg := tool.NewRegistry()

	dummyDef := tool.ToolDefinition{
		Name:        "dummy",
		Description: "A dummy test tool",
		Parameters:  tool.Object([]string{"input"}, map[string]tool.Property{"input": tool.String("test input")}),
	}

	dummyTool := tool.New(dummyDef, func(ctx context.Context, params struct {
		Input string `json:"input"`
	}) (string, error) {
		return "hello " + params.Input, nil
	})

	if err := reg.Register(dummyTool); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	if names := reg.Names(); len(names) != 1 || names[0] != "dummy" {
		t.Fatalf("unexpected tool names: %v", names)
	}

	res := reg.Invoke(context.Background(), tool.ToolUse{
		Type:  tool.BlockTypeToolUse,
		ID:    "call_1",
		Name:  "dummy",
		Input: json.RawMessage(`{"input": "world"}`),
	})

	if res.IsError {
		t.Fatalf("invoke failed: %v", res)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "hello world" {
		t.Fatalf("unexpected result: %v", res.Content)
	}
}
