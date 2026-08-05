package tool

import (
	"encoding/json"
	"fmt"
)

// Block type discriminators used by the Messages API.
const (
	BlockTypeText       = "text"
	BlockTypeToolUse    = "tool_use"
	BlockTypeToolResult = "tool_result"
)

// ToolDefinition is a function declaration in the Interactions API tool spec.
type ToolDefinition struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Parameters  Parameters `json:"parameters"`
}

// Parameters is the JSON Schema describing a tool's parameters.
type Parameters struct {
	Type                 string              `json:"type"`
	Properties           map[string]Property `json:"properties"`
	Required             []string            `json:"required"`
	AdditionalProperties bool                `json:"additionalProperties"`
	ExtraFields          map[string]any      `json:"-"`
}

func (s Parameters) MarshalJSON() ([]byte, error) {
	if s.Type == "" {
		s.Type = "object"
	}
	if s.Properties == nil {
		s.Properties = map[string]Property{}
	}
	if s.Required == nil {
		s.Required = []string{}
	}
	type plain Parameters
	return marshalWithExtra(plain(s), s.ExtraFields)
}

// Property is one parameter's schema.
type Property struct {
	Type        string              `json:"type"`
	Description string              `json:"description,omitempty"`
	Enum        []string            `json:"enum,omitempty"`
	Items       *Property           `json:"items,omitempty"`
	Properties  map[string]Property `json:"properties,omitempty"`
	ExtraFields map[string]any      `json:"-"`
}

func (p Property) MarshalJSON() ([]byte, error) {
	type plain Property
	return marshalWithExtra(plain(p), p.ExtraFields)
}

func marshalWithExtra(v any, extra map[string]any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return data, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	for k, val := range extra {
		raw, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("extra field %q: %w", k, err)
		}
		merged[k] = raw
	}
	return json.Marshal(merged)
}

// ToolUse is a tool_use content block from an assistant message.
type ToolUse struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Validate reports whether the block is a usable tool_use request.
func (t ToolUse) Validate() error {
	if t.Type != "" && t.Type != BlockTypeToolUse {
		return fmt.Errorf("expected content block type %q, got %q", BlockTypeToolUse, t.Type)
	}
	if t.ID == "" {
		return fmt.Errorf("missing tool_use id")
	}
	if t.Name == "" {
		return fmt.Errorf("missing tool name")
	}
	return nil
}

// ToolResult is a tool_result content block to send back in a user message.
type ToolResult struct {
	Type      string         `json:"type"`
	ToolUseID string         `json:"tool_use_id"`
	Content   []ContentBlock `json:"content"`
	IsError   bool           `json:"is_error,omitempty"`
}

// ContentBlock is a text block inside a tool_result.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Text builds a text content block.
func Text(s string) ContentBlock {
	return ContentBlock{Type: BlockTypeText, Text: s}
}

// TextResult builds a successful tool_result carrying a single text block.
func TextResult(toolUseID, text string) ToolResult {
	return ToolResult{
		Type:      BlockTypeToolResult,
		ToolUseID: toolUseID,
		Content:   []ContentBlock{Text(text)},
	}
}

// ErrorResult builds a tool_result with is_error set.
func ErrorResult(toolUseID string, err error) ToolResult {
	r := TextResult(toolUseID, err.Error())
	r.IsError = true
	return r
}
