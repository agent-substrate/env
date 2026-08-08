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
)

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

// pathQuery returns the API path for the environment with suffix appended and
// p carried in the "path" query parameter, as the read and delete endpoints
// expect.
func (e *Env) pathQuery(suffix, p string) string {
	return e.path(suffix) + "?" + url.Values{"path": {p}}.Encode()
}

// Delete removes the environment permanently.
func (e *Env) Delete(ctx context.Context) error {
	return e.client.do(ctx, http.MethodDelete, e.path(""), nil, nil)
}

// run runs a command inside the environment and returns its captured output
// and exit code. The command is executed directly (not through a shell);
// see Shell for a shell-friendly shorthand.
func (e *Env) run(ctx context.Context, req ShellRequest) (*ShellResponse, error) {
	var res ShellResponse
	if err := e.client.do(ctx, http.MethodPost, e.path("/shell"), req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Shell runs a shell command line inside the environment.
func (e *Env) Shell(ctx context.Context, commandLine string) (*ShellResponse, error) {
	return e.run(ctx, ShellRequest{Command: commandLine})
}

// ReadFile streams the contents of the file at path inside the environment.
// The caller must close the returned reader.
func (e *Env) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	var res ReadFileResponse
	if err := e.client.do(ctx, http.MethodGet, e.pathQuery("/file", p), nil, &res); err != nil {
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
	req := WriteFileRequest{
		Path:    p,
		Mode:    strconv.FormatUint(uint64(mode.Perm()), 8),
		Content: data,
	}
	return e.client.do(ctx, http.MethodPost, e.path("/file"), req, nil)
}

// ListDir lists the entries of the directory at path inside the environment.
func (e *Env) ListDir(ctx context.Context, p string) ([]DirEntry, error) {
	var out ListDirResponse
	if err := e.client.do(ctx, http.MethodGet, e.pathQuery("/dir", p), nil, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// Stat returns information about the file or directory at path.
func (e *Env) Stat(ctx context.Context, p string) (DirEntry, error) {
	var entry DirEntry
	if err := e.client.do(ctx, http.MethodGet, e.pathQuery("/stat", p), nil, &entry); err != nil {
		return DirEntry{}, err
	}
	return entry, nil
}

// Mkdir creates the directory at path, along with any missing parents.
func (e *Env) Mkdir(ctx context.Context, p string, mode fs.FileMode) error {
	req := MkdirRequest{
		Path: p,
		Mode: strconv.FormatUint(uint64(mode.Perm()), 8),
	}
	return e.client.do(ctx, http.MethodPost, e.path("/dir"), req, nil)
}

// Remove deletes the file or directory tree at path.
func (e *Env) Remove(ctx context.Context, p string) error {
	return e.client.do(ctx, http.MethodDelete, e.pathQuery("/file", p), nil, nil)
}
