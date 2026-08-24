package service_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/env/env"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/guest"
	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/service"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func newAPI(t *testing.T) (*httptest.Server, *fakerouter.Router, *fakecontrol.Server, *ate.Client) {
	t.Helper()

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.State(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
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
	return srv, router, control, client
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

func TestGuestProxyAndExec(t *testing.T) {
	srv, router, _, client := newAPI(t)
	t.Chdir(t.TempDir())
	sys := guestsys.New()
	h, err := (&guest.Server{}).Handler(sys)
	if err != nil {
		t.Fatal(err)
	}
	router.Register("web-1", h)

	// Create environment via direct client.
	if err := client.Create(t.Context(), ate.CreateOptions{
		ID:        "web-1",
		Template:  "default-env",
		Namespace: "envs",
	}); err != nil {
		t.Fatalf("client.Create: %v", err)
	}

	// Write and read a file through the API.
	content := base64.StdEncoding.EncodeToString([]byte("file body"))
	resp := do(t, "POST", srv.URL+"/v1/envs/web-1/file", `{"path":"app/main.txt","content":"`+content+`"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("write file status = %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", srv.URL+"/v1/envs/web-1/file?path=app/main.txt", "")
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
	resp = do(t, "POST", srv.URL+"/v1/envs/web-1/shell", `{"command":"cat app/main.txt"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cmd status = %d, want 200", resp.StatusCode)
	}
	res := decode[env.ShellResponse](t, resp)
	if res.Stdout != "file body" || res.ExitCode != 0 {
		t.Fatalf("cmd result = %+v, want stdout %q", res, "file body")
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
}
