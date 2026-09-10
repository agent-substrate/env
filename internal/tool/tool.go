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

package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool is one callable function.
type Tool interface {
	Definition() *mcp.Tool
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// New adapts a typed function into a Tool.
func New[T any](def *mcp.Tool, run func(context.Context, T) (string, error)) Tool {
	return funcTool[T]{def: def, run: run}
}

type funcTool[T any] struct {
	def *mcp.Tool
	run func(context.Context, T) (string, error)
}

func (t funcTool[T]) Definition() *mcp.Tool { return t.def }

func (t funcTool[T]) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var params T
	if len(bytes.TrimSpace(input)) > 0 {
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input for %s: %w", t.def.Name, err)
		}
	}
	return t.run(ctx, params)
}

// Registry holds the tool set and dispatches calls to it.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds tools, rejecting duplicate names.
func (r *Registry) Register(ts ...Tool) error {
	for _, t := range ts {
		name := t.Definition().Name
		if name == "" {
			return fmt.Errorf("tool has no name")
		}
		if _, dup := r.tools[name]; dup {
			return fmt.Errorf("duplicate tool %q", name)
		}
		r.tools[name] = t
	}
	return nil
}

// Names returns the registered tool names in sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Definitions returns the tool definitions in a stable (sorted) order.
func (r *Registry) Definitions() []*mcp.Tool {
	names := r.Names()
	defs := make([]*mcp.Tool, 0, len(names))
	for _, name := range names {
		defs = append(defs, r.tools[name].Definition())
	}
	return defs
}

// Invoke runs one tool and always returns an *mcp.CallToolResult.
func (r *Registry) Invoke(ctx context.Context, name string, input json.RawMessage) (result *mcp.CallToolResult) {
	defer func() {
		if v := recover(); v != nil {
			result = &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("tool %s panicked: %v\n%s", name, v, debug.Stack())}},
				IsError: true,
			}
		}
	}()

	t, ok := r.tools[name]
	if !ok {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("unknown tool %q; available: %v", name, r.Names())}},
			IsError: true,
		}
	}
	out, err := t.Run(ctx, input)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			IsError: true,
		}
	}
	if out == "" {
		out = "(no output)"
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: out}},
	}
}
