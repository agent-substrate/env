package fs

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	guestsys "github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/tool"
)

// Default limits applied when zero-initialized in Config.
const (
	DefaultMaxReadBytes  = 5 << 20  // 5 MiB
	DefaultMaxWriteBytes = 10 << 20 // 10 MiB
)

// Config configures the filesystem tool set.
type Config struct {
	ReadOnly      bool
	MaxReadBytes  int
	MaxWriteBytes int
	SkipDirs      []string
}

func (c Config) withDefaults() Config {
	if c.MaxReadBytes <= 0 {
		c.MaxReadBytes = DefaultMaxReadBytes
	}
	if c.MaxWriteBytes <= 0 {
		c.MaxWriteBytes = DefaultMaxWriteBytes
	}
	return c
}

// New returns the filesystem tool set, backed by fsSys.
func New(fsSys *guestsys.FS, cfg Config) []tool.Tool {
	cfg = cfg.withDefaults()
	ts := []tool.Tool{
		readFileTool(fsSys, cfg),
		listDirTool(fsSys, cfg),
		globTool(fsSys, cfg),
		grepTool(fsSys, cfg),
		statTool(fsSys),
	}
	if cfg.ReadOnly {
		return ts
	}
	return append(ts,
		writeFileTool(fsSys, cfg),
		editFileTool(fsSys, cfg),
		mkdirTool(fsSys),
		mvTool(fsSys),
		rmTool(fsSys),
	)
}

// --- read_file ---------------------------------------------------------------

type readFileParams struct {
	Path        string `json:"path"`
	Offset      int    `json:"offset"`
	Limit       int    `json:"limit"`
	LineNumbers *bool  `json:"line_numbers"`
}

func readFileTool(fsSys *guestsys.FS, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "read_file",
		Description: "Read a text file from the workspace. Returns the file contents with line " +
			"numbers by default, so you can quote exact line ranges back in edits. Use offset " +
			"and limit to page through a file that is too large to read at once. Rejects " +
			"binary files.",
		Parameters: tool.Object([]string{"path"}, map[string]tool.Property{
			"path":         tool.String("File path relative to the workspace root."),
			"offset":       tool.Integer("1-based line number to start reading from. Defaults to 1."),
			"limit":        tool.Integer("Maximum number of lines to return. Defaults to all remaining lines."),
			"line_numbers": tool.Boolean("Include 1-based line numbers at the start of each line. Defaults to true."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p readFileParams) (string, error) {
		lineNumbers := true
		if p.LineNumbers != nil {
			lineNumbers = *p.LineNumbers
		}
		return fsSys.ReadFileText(p.Path, p.Offset, p.Limit, lineNumbers, cfg.MaxReadBytes)
	})
}

// --- write_file --------------------------------------------------------------

type writeFileParams struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

func writeFileTool(fsSys *guestsys.FS, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "write_file",
		Description: "Write a text file in the workspace, creating parent directories as needed. " +
			"By default overwrites existing content unless append is true. Prefers small, " +
			"focused writes.",
		Parameters: tool.Object([]string{"path", "content"}, map[string]tool.Property{
			"path":    tool.String("File path relative to the workspace root."),
			"content": tool.String("Full text content to write."),
			"append":  tool.Boolean("If true, append to existing file instead of overwriting. Defaults to false."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p writeFileParams) (string, error) {
		abs, err := fsSys.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		_, existed, err := fsSys.WriteFile(p.Path, []byte(p.Content), 0o644, true, p.Append, int64(cfg.MaxWriteBytes))
		if err != nil {
			return "", err
		}
		verb := "Created"
		switch {
		case p.Append:
			verb = "Appended to"
		case existed:
			verb = "Overwrote"
		}
		return fmt.Sprintf("%s %s (%d bytes, %d lines).", verb, fsSys.Rel(abs), len(p.Content), guestsys.CountLines(p.Content)), nil
	})
}

// --- edit_file ---------------------------------------------------------------

type editFileParams struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func editFileTool(fsSys *guestsys.FS, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "edit_file",
		Description: "Edit a text file by replacing exact occurrences of old_string with new_string. " +
			"old_string must match uniquely in the file unless replace_all is set to true. " +
			"Always prefer edit_file over write_file when updating an existing file.",
		Parameters: tool.Object([]string{"path", "old_string", "new_string"}, map[string]tool.Property{
			"path":        tool.String("File path relative to the workspace root."),
			"old_string":  tool.String("Exact string sequence to replace."),
			"new_string":  tool.String("Exact replacement string sequence."),
			"replace_all": tool.Boolean("Replace all occurrences of old_string instead of requiring a unique match. Defaults to false."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p editFileParams) (string, error) {
		rel, count, err := fsSys.EditFile(p.Path, p.OldString, p.NewString, p.ReplaceAll, cfg.MaxWriteBytes)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Edited %s (%d replacement(s) made).", rel, count), nil
	})
}

// --- list_dir ----------------------------------------------------------------

type listDirParams struct {
	Path          string `json:"path"`
	Recursive     bool   `json:"recursive"`
	IncludeHidden *bool  `json:"include_hidden"`
	MaxEntries    int    `json:"max_entries"`
}

func listDirTool(fsSys *guestsys.FS, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "list_dir",
		Description: "List the contents of a directory. Directories are suffixed with a slash. " +
			"Set recursive to walk the whole subtree; noisy directories such as .git and " +
			"node_modules are automatically skipped.",
		Parameters: tool.Object([]string{"path"}, map[string]tool.Property{
			"path":           tool.String("Directory path relative to the workspace root."),
			"recursive":      tool.Boolean("Walk subdirectories. Defaults to false."),
			"include_hidden": tool.Boolean("Include entries starting with a dot. Defaults to false."),
			"max_entries":    tool.Integer("Maximum entries to return (max 1000). Defaults to 1000."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p listDirParams) (string, error) {
		includeHidden := false
		if p.IncludeHidden != nil {
			includeHidden = *p.IncludeHidden
		}
		entries, truncated, err := fsSys.ListDir(p.Path, p.Recursive, includeHidden, p.MaxEntries, cfg.SkipDirs)
		if err != nil {
			return "", err
		}
		abs, err := fsSys.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		rel := fsSys.Rel(abs)
		if len(entries) == 0 {
			return fmt.Sprintf("%s is empty.", rel), nil
		}
		lines := make([]string, 0, len(entries))
		for _, e := range entries {
			relPath := fsSys.Rel(e.Path)
			if e.IsDir {
				lines = append(lines, relPath+"/")
			} else {
				lines = append(lines, fmt.Sprintf("%s (%s)", relPath, guestsys.HumanBytes(e.Size)))
			}
		}
		sort.Strings(lines)
		out := fmt.Sprintf("%s (%d entries)\n%s", rel, len(lines), strings.Join(lines, "\n"))
		if truncated {
			limit := guestsys.ClampLimit(p.MaxEntries, 1000)
			out += fmt.Sprintf("\n[truncated at %d entries]", limit)
		}
		return out, nil
	})
}

// --- glob --------------------------------------------------------------------

type globParams struct {
	Path       string `json:"path"`
	Pattern    string `json:"pattern"`
	MaxResults int    `json:"max_results"`
}

func globTool(fsSys *guestsys.FS, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "glob",
		Description: "Search for files matching a glob pattern relative to path. Supports * within a " +
			"segment and ** to cross directories, e.g. **/*.go or cmd/**/main.go. Use this to " +
			"locate files when you know part of their name or extension.",
		Parameters: tool.Object([]string{"pattern"}, map[string]tool.Property{
			"path":        tool.String("Base directory to search under. Defaults to workspace root."),
			"pattern":     tool.String("Glob pattern, e.g. **/*.go. Matched against paths relative to the search directory."),
			"max_results": tool.Integer("Maximum matching files to return (max 1000). Defaults to 1000."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p globParams) (string, error) {
		relBase, matches, truncated, err := fsSys.Glob(p.Path, p.Pattern, p.MaxResults, cfg.SkipDirs)
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return fmt.Sprintf("No files match %q under %s.", p.Pattern, relBase), nil
		}
		out := fmt.Sprintf("%d file(s) matching %q:\n%s", len(matches), p.Pattern, strings.Join(matches, "\n"))
		if truncated {
			limit := guestsys.ClampLimit(p.MaxResults, 1000)
			out += fmt.Sprintf("\n[truncated at %d results]", limit)
		}
		return out, nil
	})
}

// --- grep --------------------------------------------------------------------

type grepParams struct {
	Path       string `json:"path"`
	Pattern    string `json:"pattern"`
	Include    string `json:"include"`
	MaxResults int    `json:"max_results"`
}

func grepTool(fsSys *guestsys.FS, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "grep",
		Description: "Search file contents for a regular expression (RE2 syntax). Returns matching " +
			"lines formatted as path:line:content. Skips binary files and noisy directories such as .git and node_modules.",
		Parameters: tool.Object([]string{"pattern"}, map[string]tool.Property{
			"path":        tool.String("Base directory or file to search. Defaults to workspace root."),
			"pattern":     tool.String("Regular expression to match against each line."),
			"include":     tool.String("Optional glob pattern (e.g. *.go or **/*.ts) to restrict which files are searched."),
			"max_results": tool.Integer("Maximum matching lines to return across all files (max 1000). Defaults to 1000."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p grepParams) (string, error) {
		return fsSys.Grep(ctx, p.Path, p.Pattern, p.Include, p.MaxResults, cfg.SkipDirs)
	})
}

// --- stat --------------------------------------------------------------------

type statParams struct {
	Path string `json:"path"`
}

func statTool(fsSys *guestsys.FS) tool.Tool {
	def := tool.ToolDefinition{
		Name:        "stat",
		Description: "Return file metadata: type, size, permissions, and modification time.",
		Parameters: tool.Object([]string{"path"}, map[string]tool.Property{
			"path": tool.String("File or directory path relative to the workspace root."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p statParams) (string, error) {
		abs, err := fsSys.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		info, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Sprintf("%s does not exist.", fsSys.Rel(abs)), nil
			}
			return "", err
		}
		kind := "file"
		switch {
		case info.IsDir():
			kind = "directory"
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
		}
		return fmt.Sprintf("%s\ntype: %s\nsize: %s (%d bytes)\nmode: %s\nmodified: %s",
			fsSys.Rel(abs), kind, guestsys.HumanBytes(info.Size()), info.Size(),
			info.Mode().String(), info.ModTime().UTC().Format(time.RFC3339)), nil
	})
}

// --- mkdir -------------------------------------------------------------------

type mkdirParams struct {
	Path string `json:"path"`
}

func mkdirTool(fsSys *guestsys.FS) tool.Tool {
	def := tool.ToolDefinition{
		Name:        "mkdir",
		Description: "Create a directory, including any missing parent directories. Succeeds if it already exists.",
		Parameters: tool.Object([]string{"path"}, map[string]tool.Property{
			"path": tool.String("Directory path relative to the workspace root."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p mkdirParams) (string, error) {
		abs, err := fsSys.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		if err := fsSys.Mkdir(p.Path, 0o755); err != nil {
			return "", err
		}
		return fmt.Sprintf("Created directory %s.", fsSys.Rel(abs)), nil
	})
}

// --- mv ----------------------------------------------------------------------

type mvParams struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Overwrite   bool   `json:"overwrite"`
}

func mvTool(fsSys *guestsys.FS) tool.Tool {
	def := tool.ToolDefinition{
		Name:        "mv",
		Description: "Move or rename a file or directory within the workspace. Refuses to overwrite an existing destination unless overwrite is set.",
		Parameters: tool.Object([]string{"source", "destination"}, map[string]tool.Property{
			"source":      tool.String("Source file or directory path relative to the workspace root."),
			"destination": tool.String("Destination file or directory path relative to the workspace root."),
			"overwrite":   tool.Boolean("If true, replace existing destination. Defaults to false."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p mvParams) (string, error) {
		srcAbs, err := fsSys.Resolve(p.Source)
		if err != nil {
			return "", err
		}
		dstAbs, err := fsSys.Resolve(p.Destination)
		if err != nil {
			return "", err
		}
		if err := fsSys.Move(p.Source, p.Destination, p.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("Moved %s to %s.", fsSys.Rel(srcAbs), fsSys.Rel(dstAbs)), nil
	})
}

// --- rm ----------------------------------------------------------------------

type rmParams struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

func rmTool(fsSys *guestsys.FS) tool.Tool {
	def := tool.ToolDefinition{
		Name:        "rm",
		Description: "Delete a file, or an empty directory. Deleting a non-empty directory requires recursive=true.",
		Parameters: tool.Object([]string{"path"}, map[string]tool.Property{
			"path":      tool.String("File or directory path relative to the workspace root."),
			"recursive": tool.Boolean("Delete a directory and everything under it. Defaults to false."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p rmParams) (string, error) {
		abs, err := fsSys.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		if abs == fsSys.Root() {
			return "", fmt.Errorf("refusing to delete the workspace root")
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return "", err
		}
		isDir := info.IsDir()
		if err := fsSys.Remove(p.Path, p.Recursive); err != nil {
			if isDir && !p.Recursive {
				return "", fmt.Errorf("%s is not empty; set recursive to delete it and its contents", fsSys.Rel(abs))
			}
			return "", err
		}
		if isDir && p.Recursive {
			return fmt.Sprintf("Deleted directory %s and its contents.", fsSys.Rel(abs)), nil
		}
		return fmt.Sprintf("Deleted %s.", fsSys.Rel(abs)), nil
	})
}
