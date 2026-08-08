// Package guest implements the HTTP server that runs inside a Substrate
// actor and provides command execution and filesystem access for the
// environment it lives in. Its state (filesystem and process memory) is
// snapshotted and restored by Substrate across suspend/resume cycles.
package guest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/agent-substrate/env/env"
	guestsys "github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/mcp"
	"github.com/agent-substrate/env/internal/tool"
	"github.com/agent-substrate/env/internal/tool/browser"
	fstool "github.com/agent-substrate/env/internal/tool/fs"
	"github.com/agent-substrate/env/internal/tool/shell"
)

// DefaultMaxFileBytes caps file content size for reads and writes.
const DefaultMaxFileBytes = 64 << 20 // 64 MiB

// Server serves the guest API. The zero value is usable with defaults.
type Server struct {
	// MaxFileBytes caps file content size for reads and writes.
	MaxFileBytes int64

	reg *tool.Registry // TODO(jbd): Remove registry.
}

// Handler returns the http.Handler serving the guest API.
func (s *Server) Handler(sys *guestsys.Sys) (http.Handler, error) {
	if sys == nil {
		sys = guestsys.New()
	}
	reg := tool.NewRegistry()
	if err := reg.Register(fstool.New(sys, fstool.Config{})...); err != nil {
		return nil, fmt.Errorf("registering fs tools: %w", err)
	}
	if err := reg.Register(shell.New(sys, shell.Config{})); err != nil {
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
	mux.HandleFunc("POST /v1/shell", func(w http.ResponseWriter, r *http.Request) { s.handleShell(sys, w, r) })
	mux.HandleFunc("GET /v1/file", func(w http.ResponseWriter, r *http.Request) { s.handleReadFile(sys, w, r) })
	mux.HandleFunc("POST /v1/file", func(w http.ResponseWriter, r *http.Request) { s.handleWriteFile(sys, w, r) })
	mux.HandleFunc("DELETE /v1/file", func(w http.ResponseWriter, r *http.Request) { s.handleDelete(sys, w, r) })
	mux.HandleFunc("GET /v1/dir", func(w http.ResponseWriter, r *http.Request) { s.handleListDir(sys, w, r) })
	mux.HandleFunc("POST /v1/dir", func(w http.ResponseWriter, r *http.Request) { s.handleMkdir(sys, w, r) })
	mux.HandleFunc("DELETE /v1/dir", func(w http.ResponseWriter, r *http.Request) { s.handleDelete(sys, w, r) })
	mux.HandleFunc("GET /v1/stat", func(w http.ResponseWriter, r *http.Request) { s.handleStat(sys, w, r) })

	mcpSrv := mcp.NewServer(reg)
	mux.HandleFunc("POST /v1/mcp", mcpSrv.ServeHTTP)

	return mux, nil
}

func (s *Server) maxFile() int64 {
	if s.MaxFileBytes > 0 {
		return s.MaxFileBytes
	}
	return DefaultMaxFileBytes
}

// resolvePath cleans p and resolves relative paths against the process
// working directory.
func (s *Server) resolvePath(sys *guestsys.Sys, p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	return sys.Resolve(p)
}

func writeError(w http.ResponseWriter, status int, code, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(env.Error{Code: code, Message: fmt.Sprintf(format, args...)})
}

func writeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, env.CodeNotFound, "%v", err)
	case errors.Is(err, syscall.EISDIR):
		writeError(w, http.StatusBadRequest, env.CodeNotFile, "%v", err)
	case errors.Is(err, syscall.ENOTDIR):
		writeError(w, http.StatusBadRequest, env.CodeNotDirectory, "%v", err)
	default:
		writeError(w, http.StatusInternalServerError, env.CodeInternal, "%v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleShell(sys *guestsys.Sys, w http.ResponseWriter, r *http.Request) {
	var req env.ShellRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "invalid request body: %v", err)
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "command is required")
		return
	}

	ctx := r.Context()
	cmd := exec.CommandContext(ctx, "sh", "-c", req.Command)
	if req.Cwd != "" {
		cwd, err := s.resolvePath(sys, req.Cwd)
		if err != nil {
			writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "invalid cwd: %v", err)
			return
		}
		cmd.Dir = cwd
	}
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

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

	err := cmd.Run()

	res := env.ShellResponse{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	switch {
	case err == nil:
		res.ExitCode = 0
	case cmd.ProcessState != nil:
		res.ExitCode = cmd.ProcessState.ExitCode()
	default:
		// The process failed to start (e.g. command not found).
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "failed to start command: %v", err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) handleReadFile(sys *guestsys.Sys, w http.ResponseWriter, r *http.Request) {
	var req env.ReadFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "decoding request body: %v", err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "path is required")
		return
	}
	path, err := s.resolvePath(sys, req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "%v", err)
		return
	}
	f, fi, err := sys.ReadFileRaw(path, s.maxFile())
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
	writeJSON(w, env.ReadFileResponse{
		Content: data,
		Mode:    "0" + strconv.FormatUint(uint64(fi.Mode().Perm()), 8),
		Size:    fi.Size(),
	})
}

func (s *Server) handleWriteFile(sys *guestsys.Sys, w http.ResponseWriter, r *http.Request) {
	var req env.WriteFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "decoding request body: %v", err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "path is required")
		return
	}
	mode := fs.FileMode(0o644)
	if req.Mode != "" {
		v, err := strconv.ParseUint(req.Mode, 8, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "invalid mode %q: %v", req.Mode, err)
			return
		}
		mode = fs.FileMode(v).Perm()
	}
	if int64(len(req.Content)) > s.maxFile() {
		writeError(w, http.StatusRequestEntityTooLarge, env.CodeInvalidArgument,
			"file content exceeds the %d byte limit", s.maxFile())
		return
	}

	if _, err := sys.WriteFile(req.Path, req.Content, mode, true, false, s.maxFile()); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDelete(sys *guestsys.Sys, w http.ResponseWriter, r *http.Request) {
	var req env.RemoveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "decoding request body: %v", err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "path is required")
		return
	}
	path, err := s.resolvePath(sys, req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "%v", err)
		return
	}
	// Report a missing path as 404; Remove itself treats it as a no-op.
	if _, err := os.Lstat(path); err != nil {
		writeFSError(w, err)
		return
	}
	if err := sys.Remove(path); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDir(sys *guestsys.Sys, w http.ResponseWriter, r *http.Request) {
	var req env.ListDirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "decoding request body: %v", err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "path is required")
		return
	}
	path, err := s.resolvePath(sys, req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "%v", err)
		return
	}
	entries, err := sys.ListDir(path, false, true, nil)
	if err != nil {
		writeFSError(w, err)
		return
	}
	writeJSON(w, env.ListDirResponse{Entries: entries})
}

func (s *Server) handleMkdir(sys *guestsys.Sys, w http.ResponseWriter, r *http.Request) {
	var req env.MkdirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "decoding request body: %v", err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "path is required")
		return
	}
	mode := fs.FileMode(0o755)
	if req.Mode != "" {
		v, err := strconv.ParseUint(req.Mode, 8, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "invalid mode %q: %v", req.Mode, err)
			return
		}
		mode = fs.FileMode(v).Perm()
	}
	if err := sys.Mkdir(req.Path, mode); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStat(sys *guestsys.Sys, w http.ResponseWriter, r *http.Request) {
	var req env.StatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "decoding request body: %v", err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "path is required")
		return
	}
	path, err := s.resolvePath(sys, req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, env.CodeInvalidArgument, "%v", err)
		return
	}
	entry, err := sys.Stat(path)
	if err != nil {
		writeFSError(w, err)
		return
	}
	writeJSON(w, entry)
}
