// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tool_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent-substrate/env/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRegistry(t *testing.T) {
	reg := tool.NewRegistry()

	dummyDef := &mcp.Tool{
		Name:        "dummy",
		Description: "A dummy test tool",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string", "description": "test input"},
			},
			"required": []string{"input"},
		},
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

	res := reg.Invoke(context.Background(), "dummy", json.RawMessage(`{"input": "world"}`))

	if res.IsError {
		t.Fatalf("invoke failed: %v", res)
	}
	if len(res.Content) != 1 {
		t.Fatalf("unexpected result length: %v", len(res.Content))
	}
	txt, ok := res.Content[0].(*mcp.TextContent)
	if !ok || txt.Text != "hello world" {
		t.Fatalf("unexpected result: %+v", res.Content[0])
	}
}
