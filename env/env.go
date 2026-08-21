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
	id       string
	atespace string
	client   *Client
}

// ID returns the environment's identifier.
func (e *Env) ID() string { return e.id }

// Atespace returns the environment's atespace.
func (e *Env) Atespace() string { return e.atespace }

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

// Suspend checkpoints and stops the environment using gRPC.
func (e *Env) Suspend(ctx context.Context) error {
	return e.client.Suspend(ctx, e.atespace, e.id)
}

// Delete removes the environment permanently using gRPC.
func (e *Env) Delete(ctx context.Context) error {
	return e.client.Delete(ctx, e.atespace, e.id)
}

// Shell runs a shell command line inside the environment.
func (e *Env) Shell(ctx context.Context, commandLine string) (*ShellResponse, error) {
	req := ShellRequest{Command: commandLine}
	var res ShellResponse
	if err := e.client.do(ctx, http.MethodPost, e.path("/shell"), req, &res); err != nil {
		return nil, err
	}
	return &res, nil
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
