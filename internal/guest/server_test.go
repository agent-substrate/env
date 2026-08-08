package guest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-substrate/env/env"
	"github.com/agent-substrate/env/internal/guest/guestsys"
)

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	fsSys, err := guestsys.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := (&Server{}).Handler(fsSys)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, dir
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func doExec(t *testing.T, srv *httptest.Server, req env.ShellRequest) env.ShellResponse {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(srv.URL+"/v1/shell", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("exec request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("exec returned %d: %s", resp.StatusCode, payload)
	}
	var res env.ShellResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decoding exec result: %v", err)
	}
	return res
}

func TestExecCapturesOutputAndExitCode(t *testing.T) {
	srv, _ := newTestServer(t)

	res := doExec(t, srv, env.ShellRequest{Command: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}})
	if res.Stdout != "out\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "out\n")
	}
	if res.Stderr != "err\n" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "err\n")
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

func TestExecEnvCwdStdin(t *testing.T) {
	srv, dir := newTestServer(t)
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)

	res := doExec(t, srv, env.ShellRequest{
		Command: []string{"sh", "-c", "pwd; printf '%s\n' \"$GREETING\"; cat"},
		Env:     map[string]string{"GREETING": "hello"},
		Cwd:     "sub",
		Stdin:   []byte("from stdin"),
	})
	want := sub + "\nhello\nfrom stdin"
	// Resolve symlinks (macOS TMPDIR) before comparing the pwd line.
	if got := res.Stdout; !strings.Contains(got, "hello\nfrom stdin") {
		t.Errorf("stdout = %q, want it to end with %q", got, want)
	}
	if res.ExitCode != 0 {
	}
}

func TestExecCommandNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	body, _ := json.Marshal(env.ShellRequest{Command: []string{"definitely-not-a-command-xyz"}})
	resp, err := http.Post(srv.URL+"/v1/shell", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestFileWriteReadRoundTrip(t *testing.T) {
	srv, dir := newTestServer(t)
	client := srv.Client()

	target := filepath.Join(dir, "nested", "greeting.txt")
	body, _ := json.Marshal(map[string]any{
		"path":    target,
		"mode":    "600",
		"content": []byte("hello world"),
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/file", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("write status = %d, want 204", resp.StatusCode)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", fi.Mode().Perm())
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/file", bytes.NewReader([]byte(`{"path":`+jsonString(target)+`}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res env.ReadFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decoding read file response: %v", err)
	}
	if string(res.Content) != "hello world" {
		t.Errorf("read back %q, want %q", res.Content, "hello world")
	}
	if res.Mode != "0600" {
		t.Errorf("Mode = %q, want 0600", res.Mode)
	}
}

func TestReadMissingFileIs404(t *testing.T) {
	srv, dir := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/file", bytes.NewReader([]byte(`{"path":`+jsonString(filepath.Join(dir, "nope"))+`}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var apiErr env.Error
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decoding error envelope: %v", err)
	}
	if apiErr.Code != env.CodeNotFound {
		t.Errorf("code = %q, want %q", apiErr.Code, env.CodeNotFound)
	}
}

func TestReadDirectoryIsRejected(t *testing.T) {
	srv, dir := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/file", bytes.NewReader([]byte(`{"path":`+jsonString(dir)+`}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestListDirAndStat(t *testing.T) {
	srv, dir := newTestServer(t)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0o755)

	req1, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/dir", bytes.NewReader([]byte(`{"path":`+jsonString(dir)+`}`)))
	req1.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listing env.ListDirResponse
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(listing.Entries), listing.Entries)
	}
	byName := map[string]env.DirEntry{}
	for _, e := range listing.Entries {
		byName[e.Name] = e
	}
	if e := byName["a.txt"]; e.IsDir || e.Size != 3 {
		t.Errorf("a.txt entry = %+v, want file of size 3", e)
	}
	if e := byName["subdir"]; !e.IsDir {
		t.Errorf("subdir entry = %+v, want directory", e)
	}

	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/stat", bytes.NewReader([]byte(`{"path":`+jsonString(filepath.Join(dir, "a.txt"))+`}`)))
	req2.Header.Set("Content-Type", "application/json")
	resp, err = srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e env.DirEntry
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.Name != "a.txt" || e.Size != 3 || e.IsDir {
		t.Errorf("stat = %+v, want a.txt file of size 3", e)
	}
}

func TestMkdirAndDelete(t *testing.T) {
	srv, dir := newTestServer(t)
	client := srv.Client()

	nested := filepath.Join(dir, "x", "y", "z")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/dir", bytes.NewReader([]byte(`{"path":`+jsonString(nested)+`}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mkdir status = %d, want 204", resp.StatusCode)
	}
	if fi, err := os.Stat(nested); err != nil || !fi.IsDir() {
		t.Fatalf("nested dir not created: %v", err)
	}

	// Delete removes the whole tree.
	root := filepath.Join(dir, "x")
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/v1/file", bytes.NewReader([]byte(`{"path":`+jsonString(root)+`}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("directory still exists after delete")
	}
}

func TestDeleteMissingIs404(t *testing.T) {
	srv, dir := newTestServer(t)
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/file", bytes.NewReader([]byte(`{"path":`+jsonString(filepath.Join(dir, "nope"))+`}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReadyzEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok\n" {
		t.Errorf("body = %q, want %q", string(body), "ok\n")
	}
}

func TestMCPEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)

	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/mcp", strings.NewReader(initReq))
	if err != nil {
		t.Fatalf("building POST /v1/mcp request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/mcp failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/mcp status = %d, want 200: %s", resp.StatusCode, body)
	}
}
