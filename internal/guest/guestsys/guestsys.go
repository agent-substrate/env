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

	"github.com/agent-substrate/env/env"
)

var defaultSkipDirs = []string{".git", "node_modules", ".venv", "venv", "__pycache__", ".next", "dist", "build", "target", ".terraform"}

// binarySniffBytes is how much of a file's head is inspected for NUL bytes
// when deciding whether it is binary.
const binarySniffBytes = 8192

// FS performs filesystem operations inside the environment.
type FS struct{}

// New returns an FS operating on the environment's filesystem.
func New() *FS { return &FS{} }

// Resolve cleans p and makes it absolute. Relative paths are resolved against
// the process working directory.
func (s *FS) Resolve(p string) (string, error) {
	return filepath.Abs(p)
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

	head := make([]byte, binarySniffBytes)
	n, _ := io.ReadFull(f, head)
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
			return fmt.Sprintf("File %s is empty.", abs), nil
		}
		return fmt.Sprintf("File %s has %d lines; offset %d is past the end.", abs, lineNo, offset), nil
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
	if oldStr == "" {
		return "", 0, fmt.Errorf("old_string must not be empty")
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
	n := strings.Count(content, oldStr)
	switch {
	case n == 0:
		return "", 0, fmt.Errorf("old_string not found in %s; read the file again to check exact content and indentation", abs)
	case n > 1 && !replaceAll:
		return "", 0, fmt.Errorf("old_string matches %d times in %s; include more context or set replace_all=true", n, abs)
	}
	count := 1
	replaced := strings.Replace(content, oldStr, newStr, 1)
	if replaceAll {
		count = n
		replaced = strings.ReplaceAll(content, oldStr, newStr)
	}
	if err := os.WriteFile(abs, []byte(replaced), 0o644); err != nil {
		return "", 0, err
	}
	return abs, count, nil
}

// Remove deletes a file or directory tree. Removing a path that does not
// exist is not an error.
func (s *FS) Remove(p string) error {
	abs, err := s.Resolve(p)
	if err != nil {
		return err
	}
	if abs == string(filepath.Separator) {
		return fmt.Errorf("refusing to delete %s", abs)
	}
	return os.RemoveAll(abs)
}

// ListDir lists directory entries, optionally recursively.
func (s *FS) ListDir(p string, recursive, includeHidden bool, maxEntries int, skipDirs []string) ([]env.DirEntry, bool, error) {
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
		return nil, false, fmt.Errorf("%s is not a directory", abs)
	}
	limit := clampLimit(maxEntries, 1000)

	var entries []env.DirEntry
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

// Glob finds matching files by pattern, newest first.
func (s *FS) Glob(ctx context.Context, p, pattern string, maxResults int, skipDirs []string) (string, []string, bool, error) {
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
	err = walkFiles(ctx, base, skipDirs, func(path string, d fs.DirEntry) error {
		rel, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return nil
		}
		if !matchGlob(pattern, filepath.ToSlash(rel)) {
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
		return "", nil, false, err
	}
	slices.SortFunc(hits, func(a, b hit) int { return b.mod.Compare(a.mod) })

	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		lines[i] = h.path
	}
	return base, lines, truncated, nil
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
		head, _ := br.Peek(binarySniffBytes)
		if isBinary(head) {
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
			lines = append(lines, fmt.Sprintf("%s:%d:%s", path, n, strings.TrimRight(scanner.Text(), "\r")))
		}
		if err := scanner.Err(); err != nil {
			lines = append(lines, fmt.Sprintf("%s: could not finish reading: %v", path, err))
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

	if len(lines) == 0 {
		return fmt.Sprintf("No matches for %q under %s.", pattern, base), nil
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
func (s *FS) Stat(p string) (env.DirEntry, error) {
	abs, err := s.Resolve(p)
	if err != nil {
		return env.DirEntry{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return env.DirEntry{}, err
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
	if srcAbs == string(filepath.Separator) {
		return fmt.Errorf("refusing to move %s", srcAbs)
	}
	if _, err := os.Stat(srcAbs); err != nil {
		return err
	}
	if _, err := os.Stat(dstAbs); err == nil && !overwrite {
		return fmt.Errorf("%s already exists; set overwrite to replace it", dstAbs)
	}
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		return err
	}
	if err := os.Rename(srcAbs, dstAbs); err != nil {
		return err
	}
	return nil
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

func buildDirEntry(path string, fi fs.FileInfo) env.DirEntry {
	return env.DirEntry{
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

// clampLimit returns requested, or max when requested is out of range.
func clampLimit(requested, max int) int {
	if requested <= 0 || requested > max {
		return max
	}
	return requested
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

// HumanBytes returns a human-readable representation of n bytes.
func HumanBytes(n int64) string {
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

// ExecOptions configures shell command execution within the environment workspace.
type ExecOptions struct {
	Command        string
	Shell          string
	Timeout        time.Duration
	MaxOutputBytes int
	Env            []string
}

// ExecShell runs a shell command in the process working directory.
func (s *FS) ExecShell(ctx context.Context, opts ExecOptions) (string, error) {
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return "", fmt.Errorf("command must not be empty")
	}

	shellPath := opts.Shell
	if shellPath == "" {
		shellPath = "/bin/sh"
	}

	timeout := opts.Timeout
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, shellPath, "-c", command)
	cmd.Env = opts.Env
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)

	if timedOut {
		return "", fmt.Errorf("timed out after %s (process group killed)", timeout)
	}

	maxBytes := opts.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}

	outStr := stdout.String()
	errStr := stderr.String()

	if len(outStr)+len(errStr) > maxBytes {
		if len(outStr) > maxBytes {
			outStr = outStr[:maxBytes]
			errStr = ""
		} else {
			errStr = errStr[:maxBytes-len(outStr)]
		}
	}

	combined := outStr + errStr
	if combined == "" && runErr != nil {
		return "", runErr
	}

	return combined, nil
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
