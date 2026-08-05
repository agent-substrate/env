package guestsys

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

var defaultSkipDirs = []string{".git", "node_modules", ".venv", "venv", "__pycache__", ".next", "dist", "build", "target", ".terraform"}

// DirEntry describes a file or directory inside the sandbox.
type DirEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	ModeString string    `json:"modeString"`
	IsDir      bool      `json:"isDir"`
	ModTime    time.Time `json:"modTime"`
}


// FS manages filesystem operations rooted inside a workspace directory.
type FS struct {
	root string
}

// New returns an FS rooted at dir, resolving symlinks and creating dir if needed.
func New(dir string) (*FS, error) {
	if dir == "" {
		dir = "/"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve root %q: %w", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return nil, fmt.Errorf("stat root %q: %w", abs, err)
		}
		info, err = os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("stat root %q: %w", abs, err)
		}
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("root %q is not a directory", abs)
	}
	return &FS{root: abs}, nil
}

// Root returns the absolute sandbox root.
func (s *FS) Root() string { return s.root }

// Resolve resolves a path relative to the root and checks for sandbox containment.
func (s *FS) Resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return s.root, nil
	}
	if strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("path %q: ~ is not expanded; use a path relative to the workspace root", p)
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.root, abs)
	}
	abs = filepath.Clean(abs)
	probe := abs
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			suffix := strings.TrimPrefix(abs, probe)
			abs = filepath.Clean(resolved + suffix)
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}

	if !s.contains(abs) {
		return "", s.escapeErr(p)
	}
	return abs, nil
}

// Rel renders an absolute path relative to the root.
func (s *FS) Rel(abs string) string {
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return abs
	}
	if rel == "." {
		return "."
	}
	return rel
}

func (s *FS) contains(abs string) bool {
	if abs == s.root {
		return true
	}
	prefix := s.root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(abs, prefix)
}

func (s *FS) escapeErr(p string) error {
	return fmt.Errorf("path %q is outside the workspace root %s", p, s.root)
}


// ReadFileRaw opens the file at path for reading.
func (s *FS) ReadFileRaw(p string, maxBytes int64) (*os.File, fs.FileInfo, error) {
	abs, err := s.Resolve(p)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if fi.IsDir() {
		f.Close()
		return nil, nil, syscall.EISDIR
	}
	if maxBytes > 0 && fi.Size() > maxBytes {
		f.Close()
		return nil, nil, fmt.Errorf("file is %d bytes, exceeds the %d byte limit", fi.Size(), maxBytes)
	}
	return f, fi, nil
}

// ReadFileText reads a text file with line numbers, offset, limit, and byte cap.
func (s *FS) ReadFileText(p string, offset, limit int, lineNumbers bool, maxBytes int) (string, error) {
	abs, err := s.Resolve(p)
	if err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()

	head := make([]byte, 8192)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		return "", fmt.Errorf("file %s is binary", s.Rel(abs))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

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
		if maxBytes > 0 && bytesReturned+len(line) > maxBytes && linesReturned > 0 {
			truncated = true
			break
		}
		out.WriteString(line)
		bytesReturned += len(line)
		linesReturned++
		if limit > 0 && linesReturned >= limit {
			if scanner.Scan() {
				truncated = true
				lineNo++
			}
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}
	if linesReturned == 0 {
		if lineNo == 0 {
			return fmt.Sprintf("File %s is empty.", s.Rel(abs)), nil
		}
		return fmt.Sprintf("File %s has %d lines; offset %d is past the end.", s.Rel(abs), lineNo, offset), nil
	}

	result := out.String()
	if truncated {
		result += fmt.Sprintf("\n[truncated at %d bytes; use offset=%d to read further]", maxBytes, lineNo)
	}
	return result, nil
}

// WriteFile writes raw bytes to path.
func (s *FS) WriteFile(p string, data []byte, mode fs.FileMode, mkdirs, appendMode bool, maxBytes int64) (int, bool, error) {
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return 0, false, fmt.Errorf("file content exceeds the %d byte limit", maxBytes)
	}
	abs, err := s.Resolve(p)
	if err != nil {
		return 0, false, err
	}
	if mkdirs {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return 0, false, err
		}
	}
	existed := false
	if _, err := os.Stat(abs); err == nil {
		existed = true
	}
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flags, mode)
	if err != nil {
		return 0, false, err
	}
	n, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return n, existed, writeErr
	}
	if closeErr != nil {
		return n, existed, closeErr
	}
	if err := os.Chmod(abs, mode); err != nil {
		return n, existed, err
	}
	return n, existed, nil
}

// EditFile replaces oldStr with newStr in a file. Returns the relative path, match count, and error.
func (s *FS) EditFile(p string, oldStr, newStr string, replaceAll bool, maxBytes int) (string, int, error) {
	if maxBytes > 0 && len(newStr) > maxBytes {
		return "", 0, fmt.Errorf("replacement is %d bytes, over the %d byte limit", len(newStr), maxBytes)
	}
	abs, err := s.Resolve(p)
	if err != nil {
		return "", 0, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", 0, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", 0, fmt.Errorf("file %s is binary", s.Rel(abs))
	}
	content := string(data)
	if oldStr == "" {
		return "", 0, fmt.Errorf("old_string must not be empty")
	}
	n := strings.Count(content, oldStr)
	switch {
	case n == 0:
		return "", 0, fmt.Errorf("old_string not found in %s; read the file again to check exact content and indentation", s.Rel(abs))
	case n > 1 && !replaceAll:
		return "", 0, fmt.Errorf("old_string matches %d times in %s; include more context or set replace_all=true", n, s.Rel(abs))
	}
	replaced := strings.Replace(content, oldStr, newStr, 1)
	if replaceAll {
		replaced = strings.ReplaceAll(content, oldStr, newStr)
	}
	if err := os.WriteFile(abs, []byte(replaced), 0o644); err != nil {
		return "", 0, err
	}
	count := 1
	if replaceAll {
		count = n
	}
	return s.Rel(abs), count, nil
}

// Remove deletes a file or directory tree.
func (s *FS) Remove(p string, recursive bool) error {
	abs, err := s.Resolve(p)
	if err != nil {
		return err
	}
	if abs == s.root {
		return fmt.Errorf("refusing to delete workspace root")
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if fi.IsDir() && !recursive {
		return syscall.EISDIR
	}
	if recursive {
		return os.RemoveAll(abs)
	}
	return os.Remove(abs)
}

// ListDir lists directory entries, optionally recursively.
func (s *FS) ListDir(p string, recursive, includeHidden bool, maxEntries int, skipDirs []string) ([]DirEntry, bool, error) {
	if skipDirs == nil {
		skipDirs = defaultSkipDirs
	}
	abs, err := s.Resolve(p)
	if err != nil {
		return nil, false, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("%s is not a directory", s.Rel(abs))
	}
	limit := clampLimit(maxEntries, 1000)

	var entries []DirEntry
	truncated := false

	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == abs {
			return nil
		}
		name := d.Name()
		if !includeHidden && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() && slices.Contains(skipDirs, name) {
			return fs.SkipDir
		}
		if len(entries) >= limit {
			truncated = true
			return fs.SkipAll
		}
		fi, fiErr := d.Info()
		if fiErr == nil {
			entries = append(entries, buildDirEntry(path, fi))
		}
		if d.IsDir() && !recursive {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil && err != fs.SkipAll {
		return nil, false, err
	}

	return entries, truncated, nil
}

// Glob finds matching files by pattern.
func (s *FS) Glob(p, pattern string, maxResults int, skipDirs []string) (string, []string, bool, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", nil, false, fmt.Errorf("pattern must not be empty")
	}
	if skipDirs == nil {
		skipDirs = defaultSkipDirs
	}
	base, err := s.Resolve(p)
	if err != nil {
		return "", nil, false, err
	}
	limit := clampLimit(maxResults, 1000)

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
			if path != base && slices.Contains(skipDirs, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return nil
		}
		if !matchGlob(pattern, filepath.ToSlash(rel)) {
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
		return "", nil, false, err
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod.After(hits[j].mod) })

	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		lines[i] = s.Rel(h.path)
	}
	return s.Rel(base), lines, truncated, nil
}

// Grep searches file contents using regex.
func (s *FS) Grep(ctx context.Context, p, pattern, include string, maxResults int, skipDirs []string) (string, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("pattern must not be empty")
	}
	if skipDirs == nil {
		skipDirs = defaultSkipDirs
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regular expression %q: %w", pattern, err)
	}
	base, err := s.Resolve(p)
	if err != nil {
		return "", err
	}
	limit := clampLimit(maxResults, 1000)

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
			lines = append(lines, fmt.Sprintf("%s:%d:%s", s.Rel(path), n, strings.TrimRight(scanner.Text(), "\r")))
		}
		if err := scanner.Err(); err != nil {
			lines = append(lines, fmt.Sprintf("%s: could not finish reading: %v", s.Rel(path), err))
		}
		return nil
	}

	info, err := os.Stat(base)
	if err != nil {
		return "", err
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
				if path != base && slices.Contains(skipDirs, d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			if include != "" {
				rel, relErr := filepath.Rel(base, path)
				if relErr != nil || !matchGlob(include, filepath.ToSlash(rel)) {
					return nil
				}
			}
			return searchFile(path)
		})
		if err != nil && err != fs.SkipAll {
			return "", err
		}
	} else if err := searchFile(base); err != nil && err != fs.SkipAll {
		return "", err
	}

	if len(lines) == 0 {
		return fmt.Sprintf("No matches for %q under %s.", pattern, s.Rel(base)), nil
	}
	out := fmt.Sprintf("%d matching line(s) in %d file(s):\n%s", len(lines), len(filesHit), strings.Join(lines, "\n"))
	if truncated {
		out += fmt.Sprintf("\n[truncated at %d matches; narrow the pattern or set include]", limit)
	}
	return out, nil
}

// Mkdir creates a directory including missing parents.
func (s *FS) Mkdir(p string, mode fs.FileMode) error {
	abs, err := s.Resolve(p)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, mode)
}

// Stat stats path.
func (s *FS) Stat(p string) (DirEntry, error) {
	abs, err := s.Resolve(p)
	if err != nil {
		return DirEntry{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return DirEntry{}, err
	}
	return buildDirEntry(abs, fi), nil
}

// Move moves/renames a file or directory.
func (s *FS) Move(src, dst string, overwrite bool) error {
	srcAbs, err := s.Resolve(src)
	if err != nil {
		return err
	}
	dstAbs, err := s.Resolve(dst)
	if err != nil {
		return err
	}
	if srcAbs == s.root {
		return fmt.Errorf("refusing to move the workspace root")
	}
	if _, err := os.Stat(srcAbs); err != nil {
		return err
	}
	if _, err := os.Stat(dstAbs); err == nil && !overwrite {
		return fmt.Errorf("%s already exists; set overwrite to replace it", s.Rel(dstAbs))
	}
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		return err
	}
	if err := os.Rename(srcAbs, dstAbs); err != nil {
		return err
	}
	return nil
}

func buildDirEntry(path string, fi fs.FileInfo) DirEntry {
	return DirEntry{
		Name:       fi.Name(),
		Path:       path,
		Size:       fi.Size(),
		Mode:       uint32(fi.Mode()),
		ModeString: fi.Mode().String(),
		IsDir:      fi.IsDir(),
		ModTime:    fi.ModTime().UTC(),
	}
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

// ClampLimit clamps requested to max if requested <= 0.
func ClampLimit(requested, max int) int {
	return clampLimit(requested, max)
}

func clampLimit(requested, max int) int {
	if requested <= 0 || requested > max {
		return max
	}
	return requested
}

// CountLines counts the number of lines in s.
func CountLines(s string) int {
	return countLines(s)
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

// HumanBytes returns a human-readable representation of n bytes.
func HumanBytes(n int64) string {
	return humanBytes(n)
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

// ExecOptions configures shell command execution within the sandbox workspace.
type ExecOptions struct {
	Command        string
	Workdir        string
	Shell          string
	Timeout        time.Duration
	MaxOutputBytes int
	Env            []string
}

// ExecShell runs a shell command inside the workspace directory.
func (s *FS) ExecShell(ctx context.Context, opts ExecOptions) (string, error) {
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return "", fmt.Errorf("command must not be empty")
	}

	shellPath := opts.Shell
	if shellPath == "" {
		shellPath = "/bin/sh"
	}

	abs, err := s.Resolve(opts.Workdir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("workdir %s is not a directory", s.Rel(abs))
	}

	timeout := opts.Timeout
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, shellPath, "-c", command)
	cmd.Dir = abs
	cmd.Env = opts.Env
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start).Round(time.Millisecond)

	var out strings.Builder
	fmt.Fprintf(&out, "$ %s\n", command)
	exitCode := cmd.ProcessState.ExitCode()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)

	switch {
	case timedOut:
		fmt.Fprintf(&out, "timed out after %s (process group killed)\n", timeout)
	case runErr != nil && exitCode < 0:
		fmt.Fprintf(&out, "failed to run: %v\n", runErr)
	default:
		fmt.Fprintf(&out, "exit code: %d (%s)\n", exitCode, elapsed)
	}

	maxBytes := opts.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	writeStream(&out, "stdout", stdout.String(), maxBytes)
	writeStream(&out, "stderr", stderr.String(), maxBytes)

	return out.String(), nil
}

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

func writeStream(out *strings.Builder, name, body string, max int) {
	if body == "" {
		return
	}
	truncated := false
	if len(body) > max {
		body, truncated = body[:max], true
	}
	fmt.Fprintf(out, "\n--- %s ---\n%s", name, body)
	if !strings.HasSuffix(body, "\n") {
		out.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(out, "[%s truncated at %d bytes]\n", name, max)
	}
}
