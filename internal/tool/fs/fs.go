package fs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/agent-substrate/sandbox/internal/tool"
)

// Config bounds what the filesystem tools will read and write.
type Config struct {
	MaxReadBytes  int
	MaxWriteBytes int
	MaxEntries    int
	ReadOnly      bool
	SkipDirs      []string
}

var defaultSkipDirs = []string{".git", "node_modules", ".venv", "venv", "__pycache__", ".next", "dist", "build", "target", ".terraform"}

func (c Config) withDefaults() Config {
	if c.MaxReadBytes <= 0 {
		c.MaxReadBytes = 256 * 1024
	}
	if c.MaxWriteBytes <= 0 {
		c.MaxWriteBytes = 8 * 1024 * 1024
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = 1000
	}
	if c.SkipDirs == nil {
		c.SkipDirs = defaultSkipDirs
	}
	return c
}

func (c Config) skip(name string) bool {
	return slices.Contains(c.SkipDirs, name)
}

// New returns the filesystem tool set, confined to sb.
func New(sb *tool.Sandbox, cfg Config) []tool.Tool {
	cfg = cfg.withDefaults()
	ts := []tool.Tool{
		readFileTool(sb, cfg),
		listDirTool(sb, cfg),
		globTool(sb, cfg),
		grepTool(sb, cfg),
		statTool(sb),
	}
	if cfg.ReadOnly {
		return ts
	}
	return append(ts,
		writeFileTool(sb, cfg),
		editFileTool(sb, cfg),
		mkdirTool(sb),
		mvTool(sb),
		rmTool(sb),
	)
}

// --- read_file ---------------------------------------------------------------

type readFileParams struct {
	Path        string `json:"path"`
	Offset      int    `json:"offset"`
	Limit       int    `json:"limit"`
	LineNumbers *bool  `json:"line_numbers"`
}

func readFileTool(sb *tool.Sandbox, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "read_file",
		Description: "Read a text file from the workspace. Returns the file contents with line " +
			"numbers by default, so you can quote exact line ranges back in edits. Use offset " +
			"and limit to page through a file that is too large to read at once. Rejects " +
			"binary files.",
		Parameters: tool.Object([]string{"path"}, map[string]tool.Property{
			"path":         tool.String("File path, relative to the workspace root."),
			"offset":       tool.Integer("1-based line number to start reading from. Omit to start at the beginning."),
			"limit":        tool.Integer("Maximum number of lines to return. Omit to read to the end (subject to a byte cap)."),
			"line_numbers": tool.Boolean("Prefix each line with its line number. Defaults to true; set false to get the raw bytes."),
		}),
	}
	return tool.New(def, func(_ context.Context, p readFileParams) (string, error) {
		abs, err := sb.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		f, err := os.Open(abs)
		if err != nil {
			return "", cleanErr(sb, err)
		}
		defer f.Close()

		head := make([]byte, 8192)
		n, _ := io.ReadFull(f, head)
		head = head[:n]
		if bytes.IndexByte(head, 0) >= 0 {
			return "", fmt.Errorf("file %s is binary", sb.Rel(abs))
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", cleanErr(sb, err)
		}

		lineNumbers := true
		if p.LineNumbers != nil {
			lineNumbers = *p.LineNumbers
		}
		offset := p.Offset
		if offset < 1 {
			offset = 1
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		var out strings.Builder
		lineNo := 0
		linesReturned := 0
		bytesReturned := 0
		truncated := false

		for scanner.Scan() {
			lineNo++
			if lineNo < offset {
				continue
			}
			text := scanner.Text()
			line := text
			if lineNumbers {
				line = fmt.Sprintf("%6d\t%s", lineNo, text)
			}
			if lineNo > offset || linesReturned > 0 {
				line = "\n" + line
			}
			if bytesReturned+len(line) > cfg.MaxReadBytes && linesReturned > 0 {
				truncated = true
				break
			}
			out.WriteString(line)
			bytesReturned += len(line)
			linesReturned++
			if p.Limit > 0 && linesReturned >= p.Limit {
				if scanner.Scan() {
					truncated = true
					lineNo++
				}
				break
			}
		}

		if err := scanner.Err(); err != nil {
			return "", cleanErr(sb, err)
		}
		if linesReturned == 0 {
			if lineNo == 0 {
				return fmt.Sprintf("File %s is empty.", sb.Rel(abs)), nil
			}
			return fmt.Sprintf("File %s has %d lines; offset %d is past the end.", sb.Rel(abs), lineNo, offset), nil
		}

		result := out.String()
		if truncated {
			result += fmt.Sprintf("\n[truncated at %d bytes; use offset=%d to read further]", cfg.MaxReadBytes, lineNo)
		}
		return result, nil
	})
}

// --- write_file --------------------------------------------------------------

type writeFileParams struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

func writeFileTool(sb *tool.Sandbox, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "write_file",
		Description: "Write a text file in the workspace, creating parent directories as needed. " +
			"By default this overwrites the whole file — to change part of an existing file, " +
			"prefer edit_file so you do not have to reproduce content you have not read.",
		Parameters: tool.Object([]string{"path", "content"}, map[string]tool.Property{
			"path":    tool.String("File path, relative to the workspace root."),
			"content": tool.String("Full contents to write."),
			"append":  tool.Boolean("Append to the file instead of overwriting it. Defaults to false."),
		}),
	}
	return tool.New(def, func(_ context.Context, p writeFileParams) (string, error) {
		if len(p.Content) > cfg.MaxWriteBytes {
			return "", fmt.Errorf("content is %d bytes, over the %d byte limit", len(p.Content), cfg.MaxWriteBytes)
		}
		abs, err := sb.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", cleanErr(sb, err)
		}
		existed := false
		if _, err := os.Stat(abs); err == nil {
			existed = true
		}
		flags := os.O_CREATE | os.O_WRONLY
		if p.Append {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(abs, flags, 0o644)
		if err != nil {
			return "", cleanErr(sb, err)
		}
		defer f.Close()

		if _, err := f.WriteString(p.Content); err != nil {
			return "", cleanErr(sb, err)
		}
		verb := "Created"
		switch {
		case p.Append:
			verb = "Appended to"
		case existed:
			verb = "Overwrote"
		}
		return fmt.Sprintf("%s %s (%d bytes, %d lines).", verb, sb.Rel(abs), len(p.Content), countLines(p.Content)), nil
	})
}

// --- edit_file ---------------------------------------------------------------

type editFileParams struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func editFileTool(sb *tool.Sandbox, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "edit_file",
		Description: "Replace an exact string in a file. old_string must match the file byte for " +
			"byte, including indentation, and must be unique unless replace_all is set — an " +
			"ambiguous or missing match is an error rather than a guess. Read the file first " +
			"so old_string reflects what is actually on disk.",
		Parameters: tool.Object([]string{"path", "old_string", "new_string"}, map[string]tool.Property{
			"path":        tool.String("File path, relative to the workspace root."),
			"old_string":  tool.String("Exact text to replace, including surrounding context to make it unique."),
			"new_string":  tool.String("Replacement text. Use an empty string to delete the matched text."),
			"replace_all": tool.Boolean("Replace every occurrence instead of requiring a unique match. Defaults to false."),
		}),
	}
	return tool.New(def, func(_ context.Context, p editFileParams) (string, error) {
		if len(p.NewString) > cfg.MaxWriteBytes {
			return "", fmt.Errorf("replacement is %d bytes, over the %d byte limit", len(p.NewString), cfg.MaxWriteBytes)
		}
		abs, err := sb.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return "", cleanErr(sb, err)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return "", fmt.Errorf("file %s is binary", sb.Rel(abs))
		}
		content := string(data)
		if p.OldString == "" {
			return "", fmt.Errorf("old_string must not be empty")
		}
		n := strings.Count(content, p.OldString)
		switch {
		case n == 0:
			return "", fmt.Errorf("old_string not found in %s; read the file again to check exact content and indentation", sb.Rel(abs))
		case n > 1 && !p.ReplaceAll:
			return "", fmt.Errorf("old_string matches %d times in %s; include more context or set replace_all=true", n, sb.Rel(abs))
		}
		replaced := strings.Replace(content, p.OldString, p.NewString, 1)
		if p.ReplaceAll {
			replaced = strings.ReplaceAll(content, p.OldString, p.NewString)
		}
		if err := os.WriteFile(abs, []byte(replaced), 0o644); err != nil {
			return "", cleanErr(sb, err)
		}
		count := 1
		if p.ReplaceAll {
			count = n
		}
		return fmt.Sprintf("Edited %s (%d replacement(s) made).", sb.Rel(abs), count), nil
	})
}

// --- list_dir ----------------------------------------------------------------

type listDirParams struct {
	Path          string `json:"path"`
	Recursive     bool   `json:"recursive"`
	IncludeHidden bool   `json:"include_hidden"`
	MaxEntries    int    `json:"max_entries"`
}

func listDirTool(sb *tool.Sandbox, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "list_dir",
		Description: "List the contents of a directory. Directories are suffixed with a slash. " +
			"Set recursive to walk the whole subtree; noisy directories such as .git and " +
			"node_modules are skipped.",
		Parameters: tool.Object(nil, map[string]tool.Property{
			"path":           tool.String("Directory path, relative to the workspace root. Defaults to the root."),
			"recursive":      tool.Boolean("Walk subdirectories. Defaults to false."),
			"include_hidden": tool.Boolean("Include dotfiles. Defaults to false."),
			"max_entries":    tool.Integer("Maximum number of entries to return."),
		}),
	}
	return tool.New(def, func(_ context.Context, p listDirParams) (string, error) {
		abs, err := sb.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", cleanErr(sb, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", sb.Rel(abs))
		}
		limit := clampLimit(p.MaxEntries, cfg.MaxEntries)

		var lines []string
		truncated := false
		add := func(path string, d fs.DirEntry) {
			name := sb.Rel(path)
			if d.IsDir() {
				lines = append(lines, name+"/")
				return
			}
			size := int64(-1)
			if fi, err := d.Info(); err == nil {
				size = fi.Size()
			}
			lines = append(lines, fmt.Sprintf("%s (%s)", name, humanBytes(size)))
		}

		if p.Recursive {
			err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if path == abs {
					return nil
				}
				name := d.Name()
				if !p.IncludeHidden && strings.HasPrefix(name, ".") {
					if d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if d.IsDir() && cfg.skip(name) {
					return fs.SkipDir
				}
				if len(lines) >= limit {
					truncated = true
					return fs.SkipAll
				}
				add(path, d)
				return nil
			})
			if err != nil {
				return "", cleanErr(sb, err)
			}
		} else {
			entries, err := os.ReadDir(abs)
			if err != nil {
				return "", cleanErr(sb, err)
			}
			for _, d := range entries {
				if !p.IncludeHidden && strings.HasPrefix(d.Name(), ".") {
					continue
				}
				if len(lines) >= limit {
					truncated = true
					break
				}
				add(filepath.Join(abs, d.Name()), d)
			}
		}

		sort.Strings(lines)
		if len(lines) == 0 {
			return fmt.Sprintf("%s is empty.", sb.Rel(abs)), nil
		}
		out := fmt.Sprintf("%s (%d entries)\n%s", sb.Rel(abs), len(lines), strings.Join(lines, "\n"))
		if truncated {
			out += fmt.Sprintf("\n[truncated at %d entries]", limit)
		}
		return out, nil
	})
}

// --- glob --------------------------------------------------------------------

type globParams struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
}

func globTool(sb *tool.Sandbox, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "glob",
		Description: "Find files by path pattern, newest first. Supports * and ? within a path " +
			"segment and ** to cross directories, e.g. **/*.go or cmd/**/main.go. Use this to " +
			"locate files by name; use grep to search their contents.",
		Parameters: tool.Object([]string{"pattern"}, map[string]tool.Property{
			"pattern":     tool.String("Glob pattern, e.g. **/*.go. Matched against paths relative to the search directory."),
			"path":        tool.String("Directory to search in, relative to the workspace root. Defaults to the root."),
			"max_results": tool.Integer("Maximum number of paths to return."),
		}),
	}
	return tool.New(def, func(_ context.Context, p globParams) (string, error) {
		if strings.TrimSpace(p.Pattern) == "" {
			return "", fmt.Errorf("pattern must not be empty")
		}
		base, err := sb.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		limit := clampLimit(p.MaxResults, cfg.MaxEntries)

		type hit struct {
			path string
			mod  time.Time
		}
		var hits []hit
		err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != base && cfg.skip(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			rel, relErr := filepath.Rel(base, path)
			if relErr != nil {
				return nil
			}
			if !matchGlob(p.Pattern, filepath.ToSlash(rel)) {
				return nil
			}
			mod := time.Time{}
			if fi, err := d.Info(); err == nil {
				mod = fi.ModTime()
			}
			hits = append(hits, hit{path: path, mod: mod})
			return nil
		})
		if err != nil {
			return "", cleanErr(sb, err)
		}
		if len(hits) == 0 {
			return fmt.Sprintf("No files match %q under %s.", p.Pattern, sb.Rel(base)), nil
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].mod.After(hits[j].mod) })

		truncated := len(hits) > limit
		if truncated {
			hits = hits[:limit]
		}
		lines := make([]string, len(hits))
		for i, h := range hits {
			lines[i] = sb.Rel(h.path)
		}
		out := fmt.Sprintf("%d file(s) matching %q:\n%s", len(lines), p.Pattern, strings.Join(lines, "\n"))
		if truncated {
			out += fmt.Sprintf("\n[truncated at %d results]", limit)
		}
		return out, nil
	})
}

func matchGlob(pattern, name string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(seg); i++ {
				if matchSegments(pat[1:], seg[i:]) {
					return true
				}
			}
			return false
		}
		if len(seg) == 0 {
			return false
		}
		ok, err := filepath.Match(pat[0], seg[0])
		if err != nil || !ok {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	return len(seg) == 0
}

// --- grep --------------------------------------------------------------------

type grepParams struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Include    string `json:"include"`
	MaxResults int    `json:"max_results"`
}

func grepTool(sb *tool.Sandbox, cfg Config) tool.Tool {
	def := tool.ToolDefinition{
		Name: "grep",
		Description: "Search text file contents for a regular expression. Returns path:line:text " +
			"matches. Skips binary files and noisy directories such as .git and node_modules.",
		Parameters: tool.Object([]string{"pattern"}, map[string]tool.Property{
			"pattern":     tool.String("Regular expression (Go / RE2 syntax)."),
			"path":        tool.String("Directory or file to search, relative to the workspace root. Defaults to the root."),
			"include":     tool.String("Glob pattern to restrict searched files, e.g. *.go or **/*.ts."),
			"max_results": tool.Integer("Maximum number of matching lines to return."),
		}),
	}
	return tool.New(def, func(ctx context.Context, p grepParams) (string, error) {
		if strings.TrimSpace(p.Pattern) == "" {
			return "", fmt.Errorf("pattern must not be empty")
		}
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return "", fmt.Errorf("invalid regular expression %q: %w", p.Pattern, err)
		}
		base, err := sb.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		limit := clampLimit(p.MaxResults, cfg.MaxEntries)

		var (
			lines     []string
			filesHit  = map[string]bool{}
			truncated bool
		)
		searchFile := func(path string) error {
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer f.Close()
			br := bufio.NewReader(f)
			head, _ := br.Peek(8192)
			if bytes.IndexByte(head, 0) >= 0 {
				return nil
			}
			scanner := bufio.NewScanner(br)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			n := 0
			for scanner.Scan() {
				n++
				if !re.MatchString(scanner.Text()) {
					continue
				}
				if len(lines) >= limit {
					truncated = true
					return fs.SkipAll
				}
				filesHit[path] = true
				lines = append(lines, fmt.Sprintf("%s:%d:%s", sb.Rel(path), n, strings.TrimRight(scanner.Text(), "\r")))
			}
			if err := scanner.Err(); err != nil {
				lines = append(lines, fmt.Sprintf("%s: could not finish reading: %v", sb.Rel(path), err))
			}
			return nil
		}

		info, err := os.Stat(base)
		if err != nil {
			return "", cleanErr(sb, err)
		}
		if info.IsDir() {
			err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if d.IsDir() {
					if path != base && cfg.skip(d.Name()) {
						return fs.SkipDir
					}
					return nil
				}
				if p.Include != "" {
					rel, relErr := filepath.Rel(base, path)
					if relErr != nil || !matchGlob(p.Include, filepath.ToSlash(rel)) {
						return nil
					}
				}
				return searchFile(path)
			})
			if err != nil && err != fs.SkipAll {
				return "", cleanErr(sb, err)
			}
		} else if err := searchFile(base); err != nil && err != fs.SkipAll {
			return "", cleanErr(sb, err)
		}

		if len(lines) == 0 {
			return fmt.Sprintf("No matches for %q under %s.", p.Pattern, sb.Rel(base)), nil
		}
		out := fmt.Sprintf("%d matching line(s) in %d file(s):\n%s", len(lines), len(filesHit), strings.Join(lines, "\n"))
		if truncated {
			out += fmt.Sprintf("\n[truncated at %d matches; narrow the pattern or set include]", limit)
		}
		return out, nil
	})
}

// --- stat --------------------------------------------------------------------

type pathParams struct {
	Path string `json:"path"`
}

func statTool(sb *tool.Sandbox) tool.Tool {
	def := tool.ToolDefinition{
		Name:        "stat",
		Description: "Report whether a path exists and, if so, its type, size, permissions, and modification time.",
		Parameters: tool.Object([]string{"path"}, map[string]tool.Property{
			"path": tool.String("Path to inspect, relative to the workspace root."),
		}),
	}
	return tool.New(def, func(_ context.Context, p pathParams) (string, error) {
		abs, err := sb.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		info, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Sprintf("%s does not exist.", sb.Rel(abs)), nil
			}
			return "", cleanErr(sb, err)
		}
		kind := "file"
		switch {
		case info.IsDir():
			kind = "directory"
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
		}
		return fmt.Sprintf("%s\ntype: %s\nsize: %s (%d bytes)\nmode: %s\nmodified: %s",
			sb.Rel(abs), kind, humanBytes(info.Size()), info.Size(),
			info.Mode().String(), info.ModTime().UTC().Format(time.RFC3339)), nil
	})
}

// --- mkdir -------------------------------------------------------------------

func mkdirTool(sb *tool.Sandbox) tool.Tool {
	def := tool.ToolDefinition{
		Name:        "mkdir",
		Description: "Create a directory, including any missing parent directories. Succeeds if it already exists.",
		Parameters: tool.Object([]string{"path"}, map[string]tool.Property{
			"path": tool.String("Directory path to create, relative to the workspace root."),
		}),
	}
	return tool.New(def, func(_ context.Context, p pathParams) (string, error) {
		abs, err := sb.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", cleanErr(sb, err)
		}
		return fmt.Sprintf("Created directory %s.", sb.Rel(abs)), nil
	})
}

// --- mv ----------------------------------------------------------------------

type moveParams struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Overwrite   bool   `json:"overwrite"`
}

func mvTool(sb *tool.Sandbox) tool.Tool {
	def := tool.ToolDefinition{
		Name:        "mv",
		Description: "Move or rename a file or directory within the workspace. Refuses to overwrite an existing destination unless overwrite is set.",
		Parameters: tool.Object([]string{"source", "destination"}, map[string]tool.Property{
			"source":      tool.String("Existing path, relative to the workspace root."),
			"destination": tool.String("New path, relative to the workspace root."),
			"overwrite":   tool.Boolean("Replace the destination if it exists. Defaults to false."),
		}),
	}
	return tool.New(def, func(_ context.Context, p moveParams) (string, error) {
		src, err := sb.Resolve(p.Source)
		if err != nil {
			return "", err
		}
		dst, err := sb.Resolve(p.Destination)
		if err != nil {
			return "", err
		}
		if src == sb.Root() {
			return "", fmt.Errorf("refusing to move the workspace root")
		}
		if _, err := os.Stat(src); err != nil {
			return "", cleanErr(sb, err)
		}
		if _, err := os.Stat(dst); err == nil && !p.Overwrite {
			return "", fmt.Errorf("%s already exists; set overwrite to replace it", sb.Rel(dst))
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", cleanErr(sb, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return "", cleanErr(sb, err)
		}
		return fmt.Sprintf("Moved %s to %s.", sb.Rel(src), sb.Rel(dst)), nil
	})
}

// --- rm ----------------------------------------------------------------------

type deleteParams struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

func rmTool(sb *tool.Sandbox) tool.Tool {
	def := tool.ToolDefinition{
		Name: "rm",
		Description: "Delete a file, or an empty directory. Deleting a non-empty directory " +
			"requires recursive, which cannot be undone — prefer mv if the content may " +
			"still be needed.",
		Parameters: tool.Object([]string{"path"}, map[string]tool.Property{
			"path":      tool.String("Path to delete, relative to the workspace root."),
			"recursive": tool.Boolean("Delete a directory and everything under it. Defaults to false."),
		}),
	}
	return tool.New(def, func(_ context.Context, p deleteParams) (string, error) {
		abs, err := sb.Resolve(p.Path)
		if err != nil {
			return "", err
		}
		if abs == sb.Root() {
			return "", fmt.Errorf("refusing to delete the workspace root")
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return "", cleanErr(sb, err)
		}
		if info.IsDir() && p.Recursive {
			if err := os.RemoveAll(abs); err != nil {
				return "", cleanErr(sb, err)
			}
			return fmt.Sprintf("Deleted directory %s and its contents.", sb.Rel(abs)), nil
		}
		if err := os.Remove(abs); err != nil {
			if info.IsDir() {
				return "", fmt.Errorf("%s is not empty; set recursive to delete it and its contents", sb.Rel(abs))
			}
			return "", cleanErr(sb, err)
		}
		return fmt.Sprintf("Deleted %s.", sb.Rel(abs)), nil
	})
}

// --- helpers -----------------------------------------------------------------

func cleanErr(sb *tool.Sandbox, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ReplaceAll(err.Error(), sb.Root()+string(filepath.Separator), "")
	return fmt.Errorf("%s", strings.ReplaceAll(msg, sb.Root(), "."))
}

func clampLimit(requested, max int) int {
	if requested <= 0 || requested > max {
		return max
	}
	return requested
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func humanBytes(n int64) string {
	if n < 0 {
		return "unknown size"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", v/unit)
}
