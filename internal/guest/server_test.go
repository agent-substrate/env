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

	"github.com/agent-substrate/sandbox/internal/guest/guestsys"
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

func doExec(t *testing.T, srv *httptest.Server, req CmdRequest) CmdResult {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(srv.URL+"/v1/cmd", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("exec request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("exec returned %d: %s", resp.StatusCode, payload)
	}
	var res CmdResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decoding exec result: %v", err)
	}
	return res
}

func TestExecCapturesOutputAndExitCode(t *testing.T) {
	srv, _ := newTestServer(t)

	res := doExec(t, srv, CmdRequest{Command: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}})
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

	res := doExec(t, srv, CmdRequest{
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
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
}

func TestExecOutputTruncation(t *testing.T) {
	h, err := (&Server{MaxOutputBytes: 10}).Handler(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res := doExec(t, srv, CmdRequest{Command: []string{"sh", "-c", "printf '0123456789ABCDEF'"}})
	if res.Stdout != "0123456789" {
		t.Errorf("stdout = %q, want first 10 bytes", res.Stdout)
	}
	if !res.StdoutTruncated {
		t.Error("StdoutTruncated = false, want true")
	}
}

func TestExecCommandNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	body, _ := json.Marshal(CmdRequest{Command: []string{"definitely-not-a-command-xyz"}})
	resp, err := http.Post(srv.URL+"/v1/cmd", "application/json", bytes.NewReader(body))
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
	data, _ := io.ReadAll(resp.Body)
	if string(data) != "hello world" {
		t.Errorf("read back %q, want %q", data, "hello world")
	}
	if got := resp.Header.Get("X-File-Mode"); got != "0600" {
		t.Errorf("X-File-Mode = %q, want 0600", got)
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
	var apiErr Error
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decoding error envelope: %v", err)
	}
	if apiErr.Code != CodeNotFound {
		t.Errorf("code = %q, want %q", apiErr.Code, CodeNotFound)
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
	var listing ListDirResponse
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(listing.Entries), listing.Entries)
	}
	byName := map[string]DirEntry{}
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
	var e DirEntry
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

func TestToolsEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)

	// GET /v1/tools
	resp, err := http.Get(srv.URL + "/v1/tools")
	if err != nil {
		t.Fatalf("GET /v1/tools failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/tools status = %d, want 200", resp.StatusCode)
	}
	var toolsResp toolsResponse
	if err := json.NewDecoder(resp.Body).Decode(&toolsResp); err != nil {
		t.Fatalf("decoding GET /v1/tools response: %v", err)
	}
	if len(toolsResp.Tools) == 0 {
		t.Fatalf("expected tools in response, got none")
	}

	// POST /v1/tools shell
	callBody := []byte(`{
		"type": "function_call",
		"id": "call_sh1",
		"name": "shell",
		"arguments": {"command": "echo test_tool"}
	}`)
	resp, err = http.Post(srv.URL+"/v1/tools", "application/json", bytes.NewReader(callBody))
	if err != nil {
		t.Fatalf("POST /v1/tools shell failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/tools status = %d, want 200", resp.StatusCode)
	}
	var resStep FunctionResult
	if err := json.NewDecoder(resp.Body).Decode(&resStep); err != nil {
		t.Fatalf("decoding function_result: %v", err)
	}
	if resStep.CallID != "call_sh1" || resStep.Name != "shell" {
		t.Errorf("unexpected step result: %+v", resStep)
	}
	if len(resStep.Result) == 0 || !strings.Contains(resStep.Result[0].Text, "test_tool") {
		t.Errorf("unexpected shell tool output: %+v", resStep.Result)
	}
}
