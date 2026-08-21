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
	"strings"
	"syscall"
	"time"
)

var defaultSkipDirs = []string{".git", "node_modules", ".venv", "venv", "__pycache__", ".next", "dist", "build", "target", ".terraform"}

const (
	// binarySniffBytes is how much of a file's head is inspected for NUL
	// bytes when deciding whether it is binary.
	binarySniffBytes = 8192

	// scannerInitialBytes and scannerMaxBytes size the buffer used to scan
	// files line by line. Lines longer than scannerMaxBytes fail the scan.
	scannerInitialBytes = 64 << 10
	scannerMaxBytes     = 1 << 20
)

// Sys performs filesystem operations inside the environment.
type Sys struct{}

// New returns a Sys operating on the environment's filesystem.
func New() *Sys { return &Sys{} }

// Resolve cleans p and makes it absolute. Relative paths are resolved against
// the process working directory.
func (s *Sys) Resolve(p string) (string, error) {
	return filepath.Abs(p)
}

// ReadFileRaw opens the file at p for reading. The caller owns the returned
// file and must close it.
func (s *Sys) ReadFileRaw(p string, maxBytes int64) (*os.File, fs.FileInfo, error) {
	abs, err := s.Resolve(p)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	switch {
	case err != nil:
	case fi.IsDir():
		err = syscall.EISDIR
	case maxBytes > 0 && fi.Size() > maxBytes:
		err = fmt.Errorf("file is %d bytes, exceeds the %d byte limit", fi.Size(), maxBytes)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}

// ReadFileText reads a text file, optionally starting at a 1-based line offset
// and prefixing each line with its number. Binary files are rejected.
func (s *Sys) ReadFileText(p string, offset int, lineNumbers bool) (string, error) {
	abs, err := s.Resolve(p)
	if err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()

	head := make([]byte, binarySniffBytes)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	if isBinary(head[:n]) {
		return "", fmt.Errorf("file %s is binary", abs)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	if offset < 1 {
		offset = 1
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scannerInitialBytes), scannerMaxBytes)

	var out strings.Builder
	lineNo := 0
	linesReturned := 0
	for scanner.Scan() {
		lineNo++
		if lineNo < offset {
			continue
		}
		line := scanner.Text()
		if lineNumbers {
			line = fmt.Sprintf("%6d\t%s", lineNo, line)
		}
		if linesReturned > 0 {
			out.WriteString("\n")
		}
		out.WriteString(line)
		linesReturned++
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	if linesReturned == 0 {
		if lineNo == 0 {
			return fmt.Sprintf("File %s is empty.", abs), nil
		}
		return fmt.Sprintf("File %s has %d lines; offset %d is past the end.", abs, lineNo, offset), nil
	}
	return out.String(), nil
}

// WriteFile writes data to the file at p, creating it with mode when it does
// not exist and applying mode to it when it does. Parent directories are
// created when mkdirs is set. Data is appended when append is set, and replaces
// the file's contents otherwise.
func (s *Sys) WriteFile(p string, data []byte, mode fs.FileMode, mkdirs, append bool) error {
	abs, err := s.Resolve(p)
	if err != nil {
		return err
	}
	if mkdirs {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if append {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(abs, flags, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err != nil {
		return err
	}
	if err := f.Close(); err == nil {
		return err
	}
	// OpenFile applies mode only when it creates the file, so set it
	// explicitly in case the file already existed.
	return os.Chmod(abs, mode)
}

// EditFile replaces oldStr with newStr in the file at p. oldStr must match
// exactly once unless replaceAll is set. It returns the absolute path of the
// edited file and the number of replacements made.
func (s *Sys) EditFile(p string, oldStr, newStr string, replaceAll bool, maxBytes int) (string, int, error) {
	if oldStr == "" {
		return "", 0, errors.New("old_string must not be empty")
	}
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
	if isBinary(data) {
		return "", 0, fmt.Errorf("file %s is binary", abs)
	}
	content := string(data)
	matches := strings.Count(content, oldStr)
	switch {
	case matches == 0:
		return "", 0, fmt.Errorf("old_string not found in %s; read the file again to check exact content and indentation", abs)
	case matches > 1 && !replaceAll:
		return "", 0, fmt.Errorf("old_string matches %d times in %s; include more context or set replace_all=true", matches, abs)
	}
	count := 1
	replaced := strings.Replace(content, oldStr, newStr, 1)
	if replaceAll {
		count = matches
		replaced = strings.ReplaceAll(content, oldStr, newStr)
	}
	// The file exists, so the mode argument is ignored and its mode is kept.
	if err := os.WriteFile(abs, []byte(replaced), 0o644); err != nil {
		return "", 0, err
	}
	return abs, count, nil
}

// Glob returns the paths under p matching pattern, most recently modified
// first.
func (s *Sys) Glob(ctx context.Context, p, pattern string, skipDirs []string) ([]string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, errors.New("pattern must not be empty")
	}
	if skipDirs == nil {
		skipDirs = defaultSkipDirs
	}
	base, err := s.Resolve(p)
	if err != nil {
		return nil, err
	}

	type hit struct {
		path string
		mod  time.Time
	}
	var hits []hit
	err = walkFiles(ctx, base, skipDirs, func(path string, d fs.DirEntry) error {
		rel, relErr := filepath.Rel(base, path)
		if relErr != nil || !matchGlob(pattern, filepath.ToSlash(rel)) {
			return nil
		}
		var mod time.Time
		if fi, err := d.Info(); err == nil {
			mod = fi.ModTime()
		}
		hits = append(hits, hit{path: path, mod: mod})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(hits, func(a, b hit) int { return b.mod.Compare(a.mod) })

	paths := make([]string, len(hits))
	for i, h := range hits {
		paths[i] = h.path
	}
	return paths, nil
}

// Grep returns the lines under p matching pattern, formatted as
// path:line:text. include optionally restricts the search to files matching a
// glob pattern.
func (s *Sys) Grep(ctx context.Context, p, pattern, include string, skipDirs []string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", errors.New("pattern must not be empty")
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

	var (
		lines    []string
		readErrs []string
		filesHit = map[string]struct{}{}
	)
	searchFile := func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		br := bufio.NewReader(f)
		head, _ := br.Peek(binarySniffBytes)
		if isBinary(head) {
			return nil
		}
		scanner := bufio.NewScanner(br)
		scanner.Buffer(make([]byte, 0, scannerInitialBytes), scannerMaxBytes)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if !re.MatchString(line) {
				continue
			}
			filesHit[path] = struct{}{}
			lines = append(lines, fmt.Sprintf("%s:%d:%s", path, lineNo, strings.TrimRight(line, "\r")))
		}
		if err := scanner.Err(); err != nil {
			readErrs = append(readErrs, fmt.Sprintf("%s: could not finish reading: %v", path, err))
		}
		return nil
	}

	info, err := os.Stat(base)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		err = walkFiles(ctx, base, skipDirs, func(path string, d fs.DirEntry) error {
			if include != "" {
				rel, relErr := filepath.Rel(base, path)
				if relErr != nil || !matchGlob(include, filepath.ToSlash(rel)) {
					return nil
				}
			}
			return searchFile(path)
		})
		if err != nil {
			return "", err
		}
	} else if err := searchFile(base); err != nil && !errors.Is(err, fs.SkipAll) {
		return "", err
	}

	var out string
	if len(lines) == 0 {
		out = fmt.Sprintf("No matches for %q under %s.", pattern, base)
	} else {
		out = fmt.Sprintf("%d matching line(s) in %d file(s):\n%s", len(lines), len(filesHit), strings.Join(lines, "\n"))
	}
	if len(readErrs) > 0 {
		out += "\n" + strings.Join(readErrs, "\n")
	}
	return out, nil
}

// walkFiles walks the tree rooted at base and calls fn for each file it finds,
// skipping directories named in skipDirs. Entries that cannot be read are
// skipped instead of failing the whole walk. fn may return fs.SkipAll to stop
// early, which is not reported as an error.
func walkFiles(ctx context.Context, base string, skipDirs []string, fn func(path string, d fs.DirEntry) error) error {
	return filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
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
		return fn(path, d)
	})
}

// isBinary reports whether data looks like binary content rather than text.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// matchGlob reports whether the slash-separated path name matches pattern.
// Unlike filepath.Match, "**" matches across any number of path segments.
func matchGlob(pattern, name string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// matchSegments reports whether the path segments seg match the pattern
// segments pat, expanding a "**" segment against any number of seg entries.
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

// CountLines counts the number of lines in s.
func CountLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

const (
	// defaultShell runs the command line when ExecOptions.Shell is empty.
	defaultShell = "/bin/sh"

	// killGracePeriod is how long a killed process group has to release the
	// output pipes before Run stops waiting on them.
	killGracePeriod = 2 * time.Second
)

// ExecOptions configures a shell command run by ExecShell.
type ExecOptions struct {
	Command string
	Shell   string
	Timeout time.Duration
	Env     []string
}

// ExecShell runs a shell command in the process working directory and returns
// its stdout followed by its stderr. A command that runs but exits non-zero is
// not an error as long as it produced output.
func (s *Sys) ExecShell(ctx context.Context, opts ExecOptions) (string, error) {
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return "", errors.New("command must not be empty")
	}

	shellPath := opts.Shell
	if shellPath == "" {
		shellPath = defaultShell
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, shellPath, "-c", command)
	cmd.Env = opts.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Run in its own process group so a timeout kills the whole tree rather
	// than just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killGracePeriod

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("timed out after %s (process group killed)", opts.Timeout)
	}

	combined := stdout.String() + stderr.String()
	if combined == "" && err != nil {
		return "", err
	}
	return combined, nil
}

// killProcessGroup kills the whole process group of cmd, falling back to the
// direct child if the group signal fails.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
