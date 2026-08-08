package service_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/guest"
	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/service"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func newAPI(t *testing.T) (*httptest.Server, *fakerouter.Router) {
	t.Helper()

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.Status(id) == ateapipb.Actor_STATUS_RUNNING
	}
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	client, err := ate.New(ate.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	srv := httptest.NewServer(service.Handler(client))
	t.Cleanup(srv.Close)
	return srv, router
}

func do(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return v
}

func TestLifecycleAndExec(t *testing.T) {
	srv, router := newAPI(t)
	fsSys, _ := guestsys.New(t.TempDir())
	h, err := (&guest.Server{}).Handler(fsSys)
	if err != nil {
		t.Fatal(err)
	}
	router.Register("web-1", h)

	// Create.
	resp := do(t, "POST", srv.URL+"/v1/envs", `{"id":"web-1","template":"default-env","namespace":"envs"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}

	// Write and read a file through the API.
	content := base64.StdEncoding.EncodeToString([]byte("file body"))
	resp = do(t, "POST", srv.URL+"/v1/envs/web-1/file", `{"path":"app/main.txt","content":"`+content+`"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("write file status = %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", srv.URL+"/v1/envs/web-1/file", `{"path":"app/main.txt"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read file status = %d, want 200", resp.StatusCode)
	}
	var resFile struct {
		Content []byte `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resFile); err != nil {
		t.Fatalf("decoding read file response: %v", err)
	}
	if string(resFile.Content) != "file body" {
		t.Fatalf("read file = %q, want %q", resFile.Content, "file body")
	}

	// Exec.
	resp = do(t, "POST", srv.URL+"/v1/envs/web-1/shell", `{"command":["sh","-c","cat app/main.txt"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cmd status = %d, want 200", resp.StatusCode)
	}
	res := decode[guest.ShellResult](t, resp)
	if res.Stdout != "file body" || res.ExitCode != 0 {
		t.Fatalf("cmd result = %+v, want stdout %q", res, "file body")
	}

	// List directory.
	resp = do(t, "GET", srv.URL+"/v1/envs/web-1/dir", `{"path":"app"}`)
	listing := decode[guest.ListDirResponse](t, resp)
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "main.txt" {
		t.Fatalf("listing = %+v, want [main.txt]", listing.Entries)
	}

	// MCP endpoint proxied through API (stateless tools/list).
	mcpReq, _ := http.NewRequest("POST", srv.URL+"/v1/envs/web-1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	mcpReq.Header.Set("Content-Type", "application/json")
	mcpReq.Header.Set("Accept", "application/json, text/event-stream")
	mcpResp, err := http.DefaultClient.Do(mcpReq)
	if err != nil {
		t.Fatalf("MCP request failed: %v", err)
	}
	if mcpResp.StatusCode != http.StatusOK {
		t.Fatalf("MCP status = %d, want 200", mcpResp.StatusCode)
	}
	mcpResp.Body.Close()

	// A file deletes cleanly through the file endpoint.
	resp = do(t, "DELETE", srv.URL+"/v1/envs/web-1/file", `{"path":"app/main.txt"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete file status = %d, want 204", resp.StatusCode)
	}

	// Delete the directory tree.
	resp = do(t, "DELETE", srv.URL+"/v1/envs/web-1/dir", `{"path":"app"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete dir status = %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", srv.URL+"/v1/envs/web-1/dir", `{"path":"app"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("list after delete status = %d, want 404", resp.StatusCode)
	}

	// Delete.
	resp = do(t, "DELETE", srv.URL+"/v1/envs/web-1", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
}

func TestCreateStartsEnv(t *testing.T) {
	srv, router := newAPI(t)
	fsSys, _ := guestsys.New(t.TempDir())
	h, err := (&guest.Server{}).Handler(fsSys)
	if err != nil {
		t.Fatal(err)
	}
	router.Register("started", h)

	// Create starts the environment.
	resp := do(t, "POST", srv.URL+"/v1/envs", `{"id":"started","template":"default-env","namespace":"envs"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("create status = %d, want 201", resp.StatusCode)
	}
}

func TestValidation(t *testing.T) {
	srv, _ := newAPI(t)

	resp := do(t, "POST", srv.URL+"/v1/envs", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create without id status = %d, want 400", resp.StatusCode)
	}
	// Omitting template and namespace falls back to the defaults.
	resp = do(t, "POST", srv.URL+"/v1/envs", `{"id":"bare"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("create with defaults status = %d, want 201", resp.StatusCode)
	}
}
