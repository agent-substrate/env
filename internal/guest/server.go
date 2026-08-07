// Package guest implements the HTTP server that runs inside a Substrate
// actor and provides command execution and filesystem access for the
// environment it lives in. Its state (filesystem and process memory) is
// snapshotted and restored by Substrate across suspend/resume cycles.
package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	guestsys "github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/mcp"
	"github.com/agent-substrate/env/internal/tool"
	"github.com/agent-substrate/env/internal/tool/browser"
	fstool "github.com/agent-substrate/env/internal/tool/fs"
	"github.com/agent-substrate/env/internal/tool/shell"
)

// DefaultMaxOutputBytes is the per-stream (stdout/stderr) cap on captured
// exec output.
const DefaultMaxOutputBytes = 10 << 20 // 10 MiB

// DefaultMaxFileBytes caps file content size for reads and writes.
const DefaultMaxFileBytes = 64 << 20 // 64 MiB

// Server serves the guest API. The zero value is usable with defaults.
type Server struct {
	// MaxOutputBytes caps captured stdout/stderr per exec, per stream.
	MaxOutputBytes int64

	// MaxFileBytes caps file content size for reads and writes.
	MaxFileBytes int64

	reg       *tool.Registry // TODO(jbd): Remove registry.
	mcpServer *mcp.Server
}

// MCPServer returns the guest's MCP server instance.
func (s *Server) MCPServer() *mcp.Server {
	return s.mcpServer
}

// Handler returns the http.Handler serving the guest API.
func (s *Server) Handler(fsSys *guestsys.FS) (http.Handler, error) {
	if fsSys == nil {
		var err error
		fsSys, err = guestsys.New("/")
		if err != nil {
			return nil, err
		}
	}
	reg := tool.NewRegistry()
	if err := reg.Register(fstool.New(fsSys, fstool.Config{})...); err != nil {
		return nil, fmt.Errorf("registering fs tools: %w", err)
	}
	if err := reg.Register(shell.New(fsSys, shell.Config{})); err != nil {
		return nil, fmt.Errorf("registering shell tool: %w", err)
	}
	if err := reg.Register(browser.New(browser.Config{})); err != nil {
		return nil, fmt.Errorf("registering browser tool: %w", err)
	}
	s.reg = reg

	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /v1/cmd", func(w http.ResponseWriter, r *http.Request) { s.handleCmd(fsSys, w, r) })
	mux.HandleFunc("GET /v1/file", func(w http.ResponseWriter, r *http.Request) { s.handleReadFile(fsSys, w, r) })
	mux.HandleFunc("POST /v1/file", func(w http.ResponseWriter, r *http.Request) { s.handleWriteFile(fsSys, w, r) })
	mux.HandleFunc("DELETE /v1/file", func(w http.ResponseWriter, r *http.Request) { s.handleDelete(fsSys, w, r) })
	mux.HandleFunc("GET /v1/dir", func(w http.ResponseWriter, r *http.Request) { s.handleListDir(fsSys, w, r) })
	mux.HandleFunc("POST /v1/dir", func(w http.ResponseWriter, r *http.Request) { s.handleMkdir(fsSys, w, r) })
	mux.HandleFunc("DELETE /v1/dir", func(w http.ResponseWriter, r *http.Request) { s.handleDelete(fsSys, w, r) })
	mux.HandleFunc("GET /v1/stat", func(w http.ResponseWriter, r *http.Request) { s.handleStat(fsSys, w, r) })

	mcpSrv := mcp.NewServer(reg)
	s.mcpServer = mcpSrv
	mux.HandleFunc("POST /mcp", mcpSrv.ServeHTTP)

	return mux, nil
}

func (s *Server) maxOutput() int64 {
	if s.MaxOutputBytes > 0 {
		return s.MaxOutputBytes
	}
	return DefaultMaxOutputBytes
}

func (s *Server) maxFile() int64 {
	if s.MaxFileBytes > 0 {
		return s.MaxFileBytes
	}
	return DefaultMaxFileBytes
}

// resolvePath cleans p and resolves relative paths against fsSys.Root().
func (s *Server) resolvePath(fsSys *guestsys.FS, p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(p) {
		base := fsSys.Root()
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), nil
}

type pathRequest struct {
	Path string `json:"path"`
}

func (s *Server) getPath(fsSys *guestsys.FS, r *http.Request) (string, error) {
	var req pathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return "", fmt.Errorf("decoding request body: %w", err)
	}
	if req.Path == "" {
		return "", errors.New("path is required")
	}
	return s.resolvePath(fsSys, req.Path)
}

func writeError(w http.ResponseWriter, status int, code, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Error{Code: code, Message: fmt.Sprintf(format, args...)})
}

func writeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, CodeNotFound, "%v", err)
	case errors.Is(err, syscall.EISDIR):
		writeError(w, http.StatusBadRequest, CodeNotFile, "%v", err)
	case errors.Is(err, syscall.ENOTDIR):
		writeError(w, http.StatusBadRequest, CodeNotDirectory, "%v", err)
	default:
		writeError(w, http.StatusInternalServerError, CodeInternal, "%v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// limitedBuffer captures up to max bytes and discards (but counts) the rest.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := b.max - int64(b.buf.Len()); remaining > 0 {
		if int64(n) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		b.buf.Write(p)
	} else if n > 0 {
		b.truncated = true
	}
	return n, nil
}

func (s *Server) handleCmd(fsSys *guestsys.FS, w http.ResponseWriter, r *http.Request) {
	var req CmdRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid request body: %v", err)
		return
	}
	if len(req.Command) == 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "command is required")
		return
	}

	ctx := r.Context()
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	if req.Cwd != "" {
		cwd, err := s.resolvePath(fsSys, req.Cwd)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid cwd: %v", err)
			return
		}
		cmd.Dir = cwd
	} else if fsSys != nil {
		cmd.Dir = fsSys.Root()
	}
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}

	stdout := &limitedBuffer{max: s.maxOutput()}
	stderr := &limitedBuffer{max: s.maxOutput()}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Run the command in its own process group so that a timeout kills the
	// whole tree, not just the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	res := CmdResult{
		Stdout:          stdout.buf.String(),
		Stderr:          stderr.buf.String(),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		TimedOut:        errors.Is(ctx.Err(), context.DeadlineExceeded),
		Duration:        elapsed.Round(time.Millisecond).String(),
	}
	switch {
	case err == nil:
		res.ExitCode = 0
	case cmd.ProcessState != nil:
		res.ExitCode = cmd.ProcessState.ExitCode()
	default:
		// The process failed to start (e.g. command not found).
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "failed to start command: %v", err)
		return
	}
	writeJSON(w, res)
}

type readFileJSONResponse struct {
	Content []byte `json:"content"`
	Mode    string `json:"mode,omitempty"`
	Size    int64  `json:"size"`
}

func (s *Server) handleReadFile(fsSys *guestsys.FS, w http.ResponseWriter, r *http.Request) {
	path, err := s.getPath(fsSys, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "%v", err)
		return
	}
	f, fi, err := fsSys.ReadFileRaw(path, s.maxFile())
	if err != nil {
		writeFSError(w, err)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		writeFSError(w, err)
		return
	}
	writeJSON(w, readFileJSONResponse{
		Content: data,
		Mode:    "0" + strconv.FormatUint(uint64(fi.Mode().Perm()), 8),
		Size:    fi.Size(),
	})
}

type writeFileJSONRequest struct {
	Path    string `json:"path"`
	Mode    string `json:"mode,omitempty"`
	Content []byte `json:"content,omitempty"`
}

func (s *Server) handleWriteFile(fsSys *guestsys.FS, w http.ResponseWriter, r *http.Request) {
	var req writeFileJSONRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "decoding request body: %v", err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "path is required")
		return
	}
	mode := fs.FileMode(0o644)
	if req.Mode != "" {
		v, err := strconv.ParseUint(req.Mode, 8, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid mode %q: %v", req.Mode, err)
			return
		}
		mode = fs.FileMode(v).Perm()
	}
	if int64(len(req.Content)) > s.maxFile() {
		writeError(w, http.StatusRequestEntityTooLarge, CodeInvalidArgument,
			"file content exceeds the %d byte limit", s.maxFile())
		return
	}

	_, _, err := fsSys.WriteFile(req.Path, req.Content, mode, true, false, s.maxFile())
	if err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDelete(fsSys *guestsys.FS, w http.ResponseWriter, r *http.Request) {
	path, err := s.getPath(fsSys, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "%v", err)
		return
	}
	if err := fsSys.Remove(path, true); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDir(fsSys *guestsys.FS, w http.ResponseWriter, r *http.Request) {
	path, err := s.getPath(fsSys, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "%v", err)
		return
	}
	entries, _, err := fsSys.ListDir(path, false, true, 0, nil)
	if err != nil {
		writeFSError(w, err)
		return
	}
	writeJSON(w, ListDirResponse{Entries: entries})
}

type mkdirRequest struct {
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"`
}

func (s *Server) handleMkdir(fsSys *guestsys.FS, w http.ResponseWriter, r *http.Request) {
	var req mkdirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "decoding request body: %v", err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "path is required")
		return
	}
	mode := fs.FileMode(0o755)
	if req.Mode != "" {
		v, err := strconv.ParseUint(req.Mode, 8, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid mode %q: %v", req.Mode, err)
			return
		}
		mode = fs.FileMode(v).Perm()
	}
	if err := fsSys.Mkdir(req.Path, mode); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStat(fsSys *guestsys.FS, w http.ResponseWriter, r *http.Request) {
	path, err := s.getPath(fsSys, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "%v", err)
		return
	}
	entry, err := fsSys.Stat(path)
	if err != nil {
		writeFSError(w, err)
		return
	}
	writeJSON(w, entry)
}


