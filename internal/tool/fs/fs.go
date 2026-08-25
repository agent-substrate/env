package fs

import (
	"context"
	"fmt"
	"os"
	"strings"

	guestsys "github.com/agent-substrate/env/internal/service/guestsys"
	"github.com/agent-substrate/env/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultMaxWriteBytes is the write size cap applied when Config leaves it
// zero.
const DefaultMaxWriteBytes = 10 << 20 // 10 MiB

// Config configures the filesystem tool set.
type Config struct {
	ReadOnly      bool
	MaxWriteBytes int
	SkipDirs      []string
}

func (c Config) withDefaults() Config {
	if c.MaxWriteBytes <= 0 {
		c.MaxWriteBytes = DefaultMaxWriteBytes
	}
	return c
}

// New returns the filesystem tool set, backed by sys.
func New(sys *guestsys.Sys, cfg Config) []tool.Tool {
	cfg = cfg.withDefaults()
	ts := []tool.Tool{
		readFileTool(sys, cfg),
		globTool(sys, cfg),
		grepTool(sys, cfg),
	}
	if cfg.ReadOnly {
		return ts
	}
	return append(ts,
		writeFileTool(sys, cfg),
		editFileTool(sys, cfg),
	)
}

// --- read_file ---------------------------------------------------------------

type readFileParams struct {
	Path        string `json:"path"`
	Offset      int    `json:"offset"`
	LineNumbers *bool  `json:"line_numbers"`
}

func readFileTool(sys *guestsys.Sys, cfg Config) tool.Tool {
	def := &mcp.Tool{
		Name: "read_file",
		Description: "Read a text file from the workspace. Returns the file contents with line " +
			"numbers by default, so you can quote exact line ranges back in edits. Use offset " +
			"to start reading partway through a file. Rejects binary files.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":         map[string]any{"type": "string", "description": "File path relative to the workspace root."},
				"offset":       map[string]any{"type": "integer", "description": "1-based line number to start reading from. Defaults to 1."},
				"line_numbers": map[string]any{"type": "boolean", "description": "Include 1-based line numbers at the start of each line. Defaults to true."},
			},
			"required": []string{"path"},
		},
	}
	return tool.New(def, func(ctx context.Context, p readFileParams) (string, error) {
		lineNumbers := true
		if p.LineNumbers != nil {
			lineNumbers = *p.LineNumbers
		}
		return sys.ReadFileText(p.Path, p.Offset, lineNumbers)
	})
}

// --- write_file --------------------------------------------------------------

type writeFileParams struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

func writeFileTool(sys *guestsys.Sys, cfg Config) tool.Tool {
	def := &mcp.Tool{
		Name: "write_file",
		Description: "Write a text file in the workspace, creating parent directories as needed. " +
			"By default overwrites existing content unless append is true. Prefers small, " +
			"focused writes.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path relative to the workspace root."},
				"content": map[string]any{"type": "string", "description": "Full text content to write."},
				"append":  map[string]any{"type": "boolean", "description": "If true, append to existing file instead of overwriting. Defaults to false."},
			},
			"required": []string{"path", "content"},
		},
	}
	return tool.New(def, func(ctx context.Context, p writeFileParams) (string, error) {
		if cfg.MaxWriteBytes > 0 && len(p.Content) > cfg.MaxWriteBytes {
			return "", fmt.Errorf("content is %d bytes, over the %d byte limit", len(p.Content), cfg.MaxWriteBytes)
		}
		abs, err := sys.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		_, statErr := os.Stat(abs)
		existed := statErr == nil
		if err := sys.WriteFile(p.Path, []byte(p.Content), 0o644, true, p.Append); err != nil {
			return "", err
		}
		verb := "Created"
		switch {
		case p.Append:
			verb = "Appended to"
		case existed:
			verb = "Overwrote"
		}
		return fmt.Sprintf("%s %s (%d bytes, %d lines).", verb, abs, len(p.Content), guestsys.CountLines(p.Content)), nil
	})
}

// --- edit_file ---------------------------------------------------------------

type editFileParams struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func editFileTool(sys *guestsys.Sys, cfg Config) tool.Tool {
	def := &mcp.Tool{
		Name: "edit_file",
		Description: "Edit a text file by replacing exact occurrences of old_string with new_string. " +
			"old_string must match uniquely in the file unless replace_all is set to true. " +
			"Always prefer edit_file over write_file when updating an existing file.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "File path relative to the workspace root."},
				"old_string":  map[string]any{"type": "string", "description": "Exact string sequence to replace."},
				"new_string":  map[string]any{"type": "string", "description": "Exact replacement string sequence."},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences of old_string instead of requiring a unique match. Defaults to false."},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
	}
	return tool.New(def, func(ctx context.Context, p editFileParams) (string, error) {
		rel, count, err := sys.EditFile(p.Path, p.OldString, p.NewString, p.ReplaceAll, cfg.MaxWriteBytes)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Edited %s (%d replacement(s) made).", rel, count), nil
	})
}

// --- glob --------------------------------------------------------------------

type globParams struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
}

func globTool(sys *guestsys.Sys, cfg Config) tool.Tool {
	def := &mcp.Tool{
		Name: "glob",
		Description: "Search for files matching a glob pattern relative to path. Supports * within a " +
			"segment and ** to cross directories, e.g. **/*.go or cmd/**/main.go. Use this to " +
			"locate files when you know part of their name or extension.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Base directory to search under. Defaults to workspace root."},
				"pattern": map[string]any{"type": "string", "description": "Glob pattern, e.g. **/*.go. Matched against paths relative to the search directory."},
			},
			"required": []string{"pattern"},
		},
	}
	return tool.New(def, func(ctx context.Context, p globParams) (string, error) {
		matches, err := sys.Glob(ctx, p.Path, p.Pattern, cfg.SkipDirs)
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			base, err := sys.Resolve(p.Path)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("No files match %q under %s.", p.Pattern, base), nil
		}
		return fmt.Sprintf("%d file(s) matching %q:\n%s", len(matches), p.Pattern, strings.Join(matches, "\n")), nil
	})
}

// --- grep --------------------------------------------------------------------

type grepParams struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
	Include string `json:"include"`
}

func grepTool(sys *guestsys.Sys, cfg Config) tool.Tool {
	def := &mcp.Tool{
		Name: "grep",
		Description: "Search file contents for a regular expression (RE2 syntax). Returns matching " +
			"lines formatted as path:line:content. Skips binary files and noisy directories such as .git and node_modules.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Base directory or file to search. Defaults to workspace root."},
				"pattern": map[string]any{"type": "string", "description": "Regular expression to match against each line."},
				"include": map[string]any{"type": "string", "description": "Optional glob pattern (e.g. *.go or **/*.ts) to restrict which files are searched."},
			},
			"required": []string{"pattern"},
		},
	}
	return tool.New(def, func(ctx context.Context, p grepParams) (string, error) {
		return sys.Grep(ctx, p.Path, p.Pattern, p.Include, cfg.SkipDirs)
	})
}
