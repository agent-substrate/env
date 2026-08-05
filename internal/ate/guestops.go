package ate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"

	"github.com/agent-substrate/sandbox/internal/guest"
)

// CmdRequest describes a command to run inside a sandbox.
type CmdRequest = guest.CmdRequest

// CmdResult is the outcome of a CmdRequest.
type CmdResult = guest.CmdResult

// DirEntry describes a file or directory inside a sandbox.
type DirEntry = guest.DirEntry

// Run runs a command inside the sandbox and returns its captured output
// and exit code. The command is executed directly (not through a shell);
// see Cmd for a shell-friendly shorthand.
func (s *Sandbox) Run(ctx context.Context, req CmdRequest) (*CmdResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encoding command request: %w", err)
	}
	resp, err := s.guestDo(ctx, http.MethodPost, "/v1/cmd", nil, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var res CmdResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("sandbox: decoding command response: %w", err)
	}
	return &res, nil
}

// Cmd runs a shell command line ("sh -c") inside the sandbox.
func (s *Sandbox) Cmd(ctx context.Context, commandLine string) (*CmdResult, error) {
	return s.Run(ctx, CmdRequest{Command: []string{"sh", "-c", commandLine}})
}

// ReadFile streams the contents of the file at path inside the sandbox.
// The caller must close the returned reader.
func (s *Sandbox) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	b, _ := json.Marshal(map[string]string{"path": path})
	resp, err := s.guestDo(ctx, http.MethodGet, "/v1/fs/file", nil, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// WriteFile writes the contents of r to the file at path inside the
// sandbox with the given permissions, creating parent directories as
// needed.
func (s *Sandbox) WriteFile(ctx context.Context, path string, r io.Reader, mode fs.FileMode) error {
	q := url.Values{
		"path":   {path},
		"mode":   {strconv.FormatUint(uint64(mode.Perm()), 8)},
		"mkdirs": {"true"},
	}
	resp, err := s.guestDo(ctx, http.MethodPut, "/v1/fs/file", q, "application/octet-stream", r)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ListDir lists the entries of the directory at path inside the sandbox.
func (s *Sandbox) ListDir(ctx context.Context, path string) ([]DirEntry, error) {
	b, _ := json.Marshal(map[string]string{"path": path})
	resp, err := s.guestDo(ctx, http.MethodGet, "/v1/fs/dir", nil, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out guest.ListDirResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("sandbox: decoding directory listing: %w", err)
	}
	return out.Entries, nil
}

// Stat returns information about the file or directory at path.
func (s *Sandbox) Stat(ctx context.Context, path string) (DirEntry, error) {
	b, _ := json.Marshal(map[string]string{"path": path})
	resp, err := s.guestDo(ctx, http.MethodGet, "/v1/fs/stat", nil, "application/json", bytes.NewReader(b))
	if err != nil {
		return DirEntry{}, err
	}
	defer resp.Body.Close()
	var entry DirEntry
	if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		return DirEntry{}, fmt.Errorf("sandbox: decoding stat response: %w", err)
	}
	return entry, nil
}

// Mkdir creates the directory at path, along with any missing parents.
func (s *Sandbox) Mkdir(ctx context.Context, path string, mode fs.FileMode) error {
	b, _ := json.Marshal(map[string]string{
		"path": path,
		"mode": strconv.FormatUint(uint64(mode.Perm()), 8),
	})
	resp, err := s.guestDo(ctx, http.MethodPost, "/v1/fs/dir", nil, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Remove deletes the file or directory tree at path.
func (s *Sandbox) Remove(ctx context.Context, path string) error {
	b, _ := json.Marshal(map[string]string{"path": path})
	resp, err := s.guestDo(ctx, http.MethodDelete, "/v1/fs/file", nil, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Tools returns the tool definitions from the sandbox.
func (s *Sandbox) Tools(ctx context.Context) ([]byte, error) {
	resp, err := s.guestDo(ctx, http.MethodGet, "/v1/tools", nil, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// CallTool executes a tool call in the sandbox.
func (s *Sandbox) CallTool(ctx context.Context, body io.Reader) ([]byte, error) {
	resp, err := s.guestDo(ctx, http.MethodPost, "/v1/tools", nil, "application/json", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// guestDo performs an HTTP request against the sandbox's guest daemon via
// the atenet router. Non-2xx responses are converted to errors.
func (s *Sandbox) guestDo(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader) (*http.Response, error) {
	if s.client.opts.RouterAddr == "" {
		return nil, errors.New("sandbox: Options.RouterAddr is required for command and filesystem operations")
	}

	u := "http://" + s.client.opts.RouterAddr + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("sandbox: building request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// The atenet router identifies the target actor by Host header.
	req.Host = s.id + "." + s.client.opts.HostSuffix

	resp, err := s.client.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox: reaching %q: %w", s.id, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var apiErr guest.Error
	if jsonErr := json.Unmarshal(payload, &apiErr); jsonErr == nil && apiErr.Message != "" {
		if apiErr.Code == guest.CodeNotFound {
			return nil, fmt.Errorf("sandbox: %q: %w: %s", s.id, ErrNotFound, apiErr.Message)
		}
		return nil, fmt.Errorf("sandbox: %q: %s", s.id, apiErr.Message)
	}
	return nil, fmt.Errorf("sandbox: %q returned HTTP %d: %s", s.id, resp.StatusCode, bytes.TrimSpace(payload))
}
