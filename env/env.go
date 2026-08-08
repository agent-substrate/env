package env

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"

	"github.com/agent-substrate/env/internal/guest"
	"github.com/agent-substrate/env/internal/service"
)

// ShellRequest describes a command to run inside a environment.
type ShellRequest = guest.ShellRequest

// ShellResult is the outcome of a ShellRequest.
type ShellResult = guest.ShellResult

// DirEntry describes a file or directory inside a environment.
type DirEntry = guest.DirEntry

// Env is a handle to a single environment.
type Env struct {
	id     string
	client *Client
}

// ID returns the environment's identifier.
func (e *Env) ID() string { return e.id }

// path returns the API path for the environment, with suffix appended.
func (e *Env) path(suffix string) string {
	return "/v1/envs/" + url.PathEscape(e.id) + suffix
}

// Resume restores the environment from its latest snapshot onto an available
// worker. It is a no-op on the control plane if the environment is already
// running.
func (e *Env) Resume(ctx context.Context) error {
	return e.client.doJSON(ctx, http.MethodPost, e.path("/resume"), nil, nil)
}

// Suspend snapshots the environment's full state (memory and filesystem) to
// external storage and frees its worker. The environment can later be resumed
// on any eligible worker.
func (e *Env) Suspend(ctx context.Context) error {
	return e.client.doJSON(ctx, http.MethodPost, e.path("/suspend"), nil, nil)
}

// Delete removes the environment permanently, suspending it first if it is
// running.
func (e *Env) Delete(ctx context.Context) error {
	return e.client.doJSON(ctx, http.MethodDelete, e.path(""), nil, nil)
}

// run runs a command inside the environment and returns its captured output
// and exit code. The command is executed directly (not through a shell);
// see Shell for a shell-friendly shorthand.
func (e *Env) run(ctx context.Context, req ShellRequest) (*ShellResult, error) {
	var res ShellResult
	if err := e.client.doJSON(ctx, http.MethodPost, e.path("/shell"), req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Shell runs a shell command line ("sh -c") inside the environment.
func (e *Env) Shell(ctx context.Context, commandLine string) (*ShellResult, error) {
	return e.run(ctx, ShellRequest{Command: []string{"sh", "-c", commandLine}})
}

// Cmd runs a shell command line ("sh -c") inside the environment. It is an alias for Shell.
func (e *Env) Cmd(ctx context.Context, commandLine string) (*ShellResult, error) {
	return e.Shell(ctx, commandLine)
}

type readFileResponse struct {
	Content []byte `json:"content"`
	Mode    string `json:"mode,omitempty"`
	Size    int64  `json:"size"`
}

// ReadFile streams the contents of the file at path inside the environment.
// The caller must close the returned reader.
func (e *Env) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	req := service.FSRequest{Path: p}
	var res readFileResponse
	if err := e.client.doJSON(ctx, http.MethodGet, e.path("/file"), req, &res); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(res.Content)), nil
}

// WriteFile writes the contents of r to the file at path inside the
// environment with the given permissions, creating parent directories as
// needed. The contents are buffered in memory.
func (e *Env) WriteFile(ctx context.Context, p string, r io.Reader, mode fs.FileMode) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("env: reading data for %q: %w", p, err)
	}
	req := service.FSRequest{
		Path:    p,
		Mode:    strconv.FormatUint(uint64(mode.Perm()), 8),
		Content: data,
	}
	return e.client.doJSON(ctx, http.MethodPost, e.path("/file"), req, nil)
}

// ListDir lists the entries of the directory at path inside the environment.
func (e *Env) ListDir(ctx context.Context, p string) ([]DirEntry, error) {
	req := service.FSRequest{Path: p}
	var out guest.ListDirResponse
	if err := e.client.doJSON(ctx, http.MethodGet, e.path("/dir"), req, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// Stat returns information about the file or directory at path.
func (e *Env) Stat(ctx context.Context, p string) (DirEntry, error) {
	req := service.FSRequest{Path: p}
	var entry DirEntry
	if err := e.client.doJSON(ctx, http.MethodGet, e.path("/stat"), req, &entry); err != nil {
		return DirEntry{}, err
	}
	return entry, nil
}

// Mkdir creates the directory at path, along with any missing parents.
func (e *Env) Mkdir(ctx context.Context, p string, mode fs.FileMode) error {
	req := service.FSRequest{
		Path: p,
		Mode: strconv.FormatUint(uint64(mode.Perm()), 8),
	}
	return e.client.doJSON(ctx, http.MethodPost, e.path("/dir"), req, nil)
}

// Remove deletes the file or directory tree at path.
func (e *Env) Remove(ctx context.Context, p string) error {
	req := service.FSRequest{Path: p}
	return e.client.doJSON(ctx, http.MethodDelete, e.path("/file"), req, nil)
}
