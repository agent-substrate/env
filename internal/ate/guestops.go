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
// needed. If r is not an io.ReadSeeker, it is buffered in memory so the
// request can be retried after an auto-resume.
func (s *Sandbox) WriteFile(ctx context.Context, path string, r io.Reader, mode fs.FileMode) error {
	body, ok := r.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("sandbox: reading data for %q: %w", path, err)
		}
		body = bytes.NewReader(data)
	}
	q := url.Values{
		"path":   {path},
		"mode":   {strconv.FormatUint(uint64(mode.Perm()), 8)},
		"mkdirs": {"true"},
	}
	resp, err := s.guestDo(ctx, http.MethodPut, "/v1/fs/file", q, "application/octet-stream", body)
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
	r, ok := body.(io.ReadSeeker)
	if !ok && body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("sandbox: reading tool request body: %w", err)
		}
		r = bytes.NewReader(b)
	}
	resp, err := s.guestDo(ctx, http.MethodPost, "/v1/tools", nil, "application/json", r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// guestDo performs an HTTP request against the sandbox's guest daemon via
// the atenet router. Non-2xx responses are converted to errors. If the
// client has AutoResume set and the router cannot reach the sandbox, the
// sandbox is resumed and the request retried once.
func (s *Sandbox) guestDo(ctx context.Context, method, path string, query url.Values, contentType string, body io.ReadSeeker) (*http.Response, error) {
	if s.client.opts.RouterAddr == "" {
		return nil, errors.New("sandbox: Options.RouterAddr is required for command and filesystem operations")
	}

	resp, err := s.guestDoOnce(ctx, method, path, query, contentType, body)
	if err == nil || !s.client.opts.AutoResume {
		return resp, err
	}
	if !isUnreachable(err) {
		return nil, err
	}

	// The router could not reach the sandbox; it may be suspended or
	// paused. Resume it and retry once.
	if resumeErr := s.Resume(ctx); resumeErr != nil {
		return nil, errors.Join(err, resumeErr)
	}
	if body != nil {
		if _, seekErr := body.Seek(0, io.SeekStart); seekErr != nil {
			return nil, errors.Join(err, seekErr)
		}
	}
	return s.guestDoOnce(ctx, method, path, query, contentType, body)
}

func (s *Sandbox) guestDoOnce(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader) (*http.Response, error) {
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
		return nil, &unreachableError{err: fmt.Errorf("sandbox: reaching %q: %w", s.id, err)}
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
	err = fmt.Errorf("sandbox: %q returned HTTP %d: %s", s.id, resp.StatusCode, bytes.TrimSpace(payload))
	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusGatewayTimeout {
		return nil, &unreachableError{err: err}
	}
	return nil, err
}

// unreachableError marks failures where the sandbox itself could not be
// reached (as opposed to the guest returning an application error), which
// makes the request eligible for the AutoResume retry.
type unreachableError struct{ err error }

func (e *unreachableError) Error() string { return e.err.Error() }
func (e *unreachableError) Unwrap() error { return e.err }

func isUnreachable(err error) bool {
	var u *unreachableError
	return errors.As(err, &u)
}
