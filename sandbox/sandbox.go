package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"

	"github.com/agent-substrate/sandbox/internal/guest"
	"github.com/agent-substrate/sandbox/internal/service"
)

// CmdRequest describes a command to run inside a sandbox.
type CmdRequest = guest.CmdRequest

// CmdResult is the outcome of a CmdRequest.
type CmdResult = guest.CmdResult

// DirEntry describes a file or directory inside a sandbox.
type DirEntry = guest.DirEntry

// Sandbox is a handle to a single sandbox.
type Sandbox struct {
	id     string
	client *Client
}

// ID returns the sandbox's identifier.
func (s *Sandbox) ID() string { return s.id }

// path returns the API path for the sandbox, with suffix appended.
func (s *Sandbox) path(suffix string) string {
	return "/v1/sandboxes/" + url.PathEscape(s.id) + suffix
}

// Resume restores the sandbox from its latest snapshot onto an available
// worker. It is a no-op on the control plane if the sandbox is already
// running.
func (s *Sandbox) Resume(ctx context.Context) error {
	return s.client.doJSON(ctx, http.MethodPost, s.path("/resume"), nil, nil)
}

// Suspend snapshots the sandbox's full state (memory and filesystem) to
// external storage and frees its worker. The sandbox can later be resumed
// on any eligible worker.
func (s *Sandbox) Suspend(ctx context.Context) error {
	return s.client.doJSON(ctx, http.MethodPost, s.path("/suspend"), nil, nil)
}

// Delete removes the sandbox permanently, suspending it first if it is
// running.
func (s *Sandbox) Delete(ctx context.Context) error {
	return s.client.doJSON(ctx, http.MethodDelete, s.path(""), nil, nil)
}

// run runs a command inside the sandbox and returns its captured output
// and exit code. The command is executed directly (not through a shell);
// see Cmd for a shell-friendly shorthand.
func (s *Sandbox) run(ctx context.Context, req CmdRequest) (*CmdResult, error) {
	var res CmdResult
	if err := s.client.doJSON(ctx, http.MethodPost, s.path("/cmd"), req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Cmd runs a shell command line ("sh -c") inside the sandbox.
func (s *Sandbox) Cmd(ctx context.Context, commandLine string) (*CmdResult, error) {
	return s.run(ctx, CmdRequest{Command: []string{"sh", "-c", commandLine}})
}

type readFileResponse struct {
	Content []byte `json:"content"`
	Mode    string `json:"mode,omitempty"`
	Size    int64  `json:"size"`
}

// ReadFile streams the contents of the file at path inside the sandbox.
// The caller must close the returned reader.
func (s *Sandbox) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	req := service.FSRequest{Path: p}
	var res readFileResponse
	if err := s.client.doJSON(ctx, http.MethodGet, s.path("/file"), req, &res); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(res.Content)), nil
}

// WriteFile writes the contents of r to the file at path inside the
// sandbox with the given permissions, creating parent directories as
// needed. The contents are buffered in memory.
func (s *Sandbox) WriteFile(ctx context.Context, p string, r io.Reader, mode fs.FileMode) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("sandbox: reading data for %q: %w", p, err)
	}
	req := service.FSRequest{
		Path:    p,
		Mode:    strconv.FormatUint(uint64(mode.Perm()), 8),
		Content: data,
	}
	return s.client.doJSON(ctx, http.MethodPost, s.path("/file"), req, nil)
}

// ListDir lists the entries of the directory at path inside the sandbox.
func (s *Sandbox) ListDir(ctx context.Context, p string) ([]DirEntry, error) {
	req := service.FSRequest{Path: p}
	var out guest.ListDirResponse
	if err := s.client.doJSON(ctx, http.MethodGet, s.path("/dir"), req, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// Stat returns information about the file or directory at path.
func (s *Sandbox) Stat(ctx context.Context, p string) (DirEntry, error) {
	req := service.FSRequest{Path: p}
	var entry DirEntry
	if err := s.client.doJSON(ctx, http.MethodGet, s.path("/stat"), req, &entry); err != nil {
		return DirEntry{}, err
	}
	return entry, nil
}

// Mkdir creates the directory at path, along with any missing parents.
func (s *Sandbox) Mkdir(ctx context.Context, p string, mode fs.FileMode) error {
	req := service.FSRequest{
		Path: p,
		Mode: strconv.FormatUint(uint64(mode.Perm()), 8),
	}
	return s.client.doJSON(ctx, http.MethodPost, s.path("/dir"), req, nil)
}

// Remove deletes the file or directory tree at path.
func (s *Sandbox) Remove(ctx context.Context, p string) error {
	req := service.FSRequest{Path: p}
	return s.client.doJSON(ctx, http.MethodDelete, s.path("/file"), req, nil)
}
