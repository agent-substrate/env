// Package guest implements the HTTP server that runs inside a Substrate
// actor and provides command execution and filesystem access for the
// sandbox it lives in. Its state (filesystem and process memory) is
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

	guestsys "github.com/agent-substrate/sandbox/internal/guest/guestsys"
	"github.com/agent-substrate/sandbox/internal/tool"
	"github.com/agent-substrate/sandbox/internal/tool/browser"
	fstool "github.com/agent-substrate/sandbox/internal/tool/fs"
	"github.com/agent-substrate/sandbox/internal/tool/shell"
)

// DefaultMaxOutputBytes is the per-stream (stdout/stderr) cap on captured
// exec output.
const DefaultMaxOutputBytes = 10 << 20 // 10 MiB

// DefaultMaxFileBytes caps file writes and reads through the files endpoint.
const DefaultMaxFileBytes = 64 << 20 // 64 MiB

// Server serves the guest API. The zero value is usable with defaults.
type Server struct {
	// MaxOutputBytes caps captured stdout/stderr per exec, per stream.
	MaxOutputBytes int64

	// MaxFileBytes caps file content size for reads and writes.
	MaxFileBytes int64

	// FS overrides the filesystem implementation. Defaults to guestsys.New("/").
	FS *guestsys.FS

	fs  *guestsys.FS
	reg *tool.Registry
}

// Handler returns the http.Handler serving the guest API.
func (s *Server) Handler() (http.Handler, error) {
	fsSys := s.FS
	if fsSys == nil {
		var err error
		fsSys, err = guestsys.New("/")
		if err != nil {
			return nil, err
		}
	}
	s.fs = fsSys
	reg := tool.NewRegistry()
	_ = reg.Register(fstool.New(fsSys, fstool.Config{})...)
	_ = reg.Register(shell.New(fsSys, shell.Config{}))
	_ = reg.Register(browser.New(browser.Config{}))
	s.reg = reg

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /v1/cmd", s.handleCmd)
	mux.HandleFunc("GET /v1/file", s.handleReadFile)
	mux.HandleFunc("POST /v1/file", s.handleWriteFile)
	mux.HandleFunc("DELETE /v1/file", s.handleDelete)
	mux.HandleFunc("GET /v1/dir", s.handleListDir)
	mux.HandleFunc("POST /v1/dir", s.handleMkdir)
	mux.HandleFunc("DELETE /v1/dir", s.handleDelete)
	mux.HandleFunc("GET /v1/stat", s.handleStat)
	mux.HandleFunc("GET /v1/tools", s.handleTools)
	mux.HandleFunc("POST /v1/tools", s.handleToolUse)
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

// resolvePath cleans p and resolves relative paths against s.getFS().Root().
func (s *Server) resolvePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(p) {
		base := s.fs.Root()
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), nil
}

type pathRequest struct {
	Path string `json:"path"`
}

func (s *Server) getPath(r *http.Request) (string, error) {
	var req pathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return "", fmt.Errorf("decoding request body: %w", err)
	}
	if req.Path == "" {
		return "", errors.New("path is required")
	}
	return s.resolvePath(req.Path)
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

func (s *Server) handleCmd(w http.ResponseWriter, r *http.Request) {
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
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid timeout: %v", err)
			return
		}
		if d > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}

	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	if req.Cwd != "" {
		cwd, err := s.resolvePath(req.Cwd)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid cwd: %v", err)
			return
		}
		cmd.Dir = cwd
	} else if fsSys := s.fs; fsSys != nil {
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

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	path, err := s.getPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "%v", err)
		return
	}
	f, fi, err := s.fs.ReadFileRaw(path, s.maxFile())
	if err != nil {
		writeFSError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-File-Mode", "0"+strconv.FormatUint(uint64(fi.Mode().Perm()), 8))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	io.Copy(w, f)
}

type writeFileJSONRequest struct {
	Path    string `json:"path"`
	Mode    string `json:"mode,omitempty"`
	Content []byte `json:"content,omitempty"`
}

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
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

	_, _, err := s.fs.WriteFile(req.Path, req.Content, mode, true, false, s.maxFile())
	if err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	path, err := s.getPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "%v", err)
		return
	}
	if err := s.fs.Remove(path, true); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDir(w http.ResponseWriter, r *http.Request) {
	path, err := s.getPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "%v", err)
		return
	}
	entries, _, err := s.fs.ListDir(path, false, true, 0, nil)
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

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
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
	if err := s.fs.Mkdir(req.Path, mode); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	path, err := s.getPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "%v", err)
		return
	}
	entry, err := s.fs.Stat(path)
	if err != nil {
		writeFSError(w, err)
		return
	}
	writeJSON(w, entry)
}

type toolsResponse struct {
	Tools []tool.ToolDefinition `json:"tools"`
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, toolsResponse{Tools: s.reg.Definitions()})
}

func (s *Server) handleToolUse(w http.ResponseWriter, r *http.Request) {
	var step FunctionCall
	if err := json.NewDecoder(r.Body).Decode(&step); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("invalid JSON body: %v", err),
			},
		})
		return
	}

	callID := step.CallID
	if callID == "" {
		callID = step.ID
	}

	rawArgs := step.Arguments
	if len(rawArgs) == 0 {
		rawArgs = step.Args
	}
	if len(rawArgs) == 0 {
		rawArgs = step.Input
	}
	if len(rawArgs) == 0 {
		rawArgs = json.RawMessage("{}")
	}

	tu := tool.ToolUse{
		ID:    callID,
		Name:  step.Name,
		Input: rawArgs,
	}

	res := s.reg.Invoke(r.Context(), tu)

	parts := make([]InteractionContent, len(res.Content))
	for i, c := range res.Content {
		parts[i] = InteractionContent{
			Type: c.Type,
			Text: c.Text,
		}
	}

	out := FunctionResult{
		Type:    "function_result",
		Name:    step.Name,
		CallID:  callID,
		Result:  parts,
		IsError: res.IsError,
	}

	writeJSON(w, out)
}
