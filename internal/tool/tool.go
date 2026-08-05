package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sort"
)

// Tool is one callable function.
type Tool interface {
	Definition() ToolDefinition
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// New adapts a typed function into a Tool.
func New[T any](def ToolDefinition, run func(context.Context, T) (string, error)) Tool {
	return funcTool[T]{def: def, run: run}
}

type funcTool[T any] struct {
	def ToolDefinition
	run func(context.Context, T) (string, error)
}

func (t funcTool[T]) Definition() ToolDefinition { return t.def }

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
func (r *Registry) Definitions() []ToolDefinition {
	names := r.Names()
	defs := make([]ToolDefinition, 0, len(names))
	for _, name := range names {
		defs = append(defs, r.tools[name].Definition())
	}
	return defs
}

// Invoke runs one tool_use block and always returns a tool_result for it.
func (r *Registry) Invoke(ctx context.Context, tu ToolUse) (result ToolResult) {
	defer func() {
		if v := recover(); v != nil {
			result = ErrorResult(tu.ID,
				fmt.Errorf("tool %s panicked: %v\n%s", tu.Name, v, debug.Stack()))
		}
	}()

	if err := tu.Validate(); err != nil {
		return ErrorResult(tu.ID, err)
	}
	tool, ok := r.tools[tu.Name]
	if !ok {
		return ErrorResult(tu.ID, fmt.Errorf("unknown tool %q; available: %v", tu.Name, r.Names()))
	}
	out, err := tool.Run(ctx, tu.Input)
	if err != nil {
		return ErrorResult(tu.ID, err)
	}
	if out == "" {
		out = "(no output)"
	}
	return TextResult(tu.ID, out)
}

// --- JSON Schema helpers -----------------------------------------------------

func Object(required []string, props map[string]Property) Parameters {
	if required == nil {
		required = []string{}
	}
	return Parameters{Properties: props, Required: required}
}

func String(desc string) Property {
	return Property{Type: "string", Description: desc}
}

func Enum(desc string, values ...string) Property {
	return Property{Type: "string", Description: desc, Enum: values}
}

func Integer(desc string) Property {
	return Property{Type: "integer", Description: desc}
}

func Boolean(desc string) Property {
	return Property{Type: "boolean", Description: desc}
}
