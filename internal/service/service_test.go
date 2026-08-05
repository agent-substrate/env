package service_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/agent-substrate/sandbox/internal/ate"
	"github.com/agent-substrate/sandbox/internal/guest"
	"github.com/agent-substrate/sandbox/internal/guest/guestsys"
	"github.com/agent-substrate/sandbox/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/sandbox/internal/internaltest/fakerouter"
	"github.com/agent-substrate/sandbox/internal/service"
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
	h, err := (&guest.Server{FS: fsSys}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	router.Register("web-1", h)

	// Create.
	resp := do(t, "POST", srv.URL+"/v1/sandboxes", `{"id":"web-1","template":"default","namespace":"sandboxes"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	created := decode[service.SandboxInfo](t, resp)
	if created.ID != "web-1" || created.Status != "running" {
		t.Fatalf("created = %+v, want web-1 running", created)
	}

	// Write and read a file through the API.
	content := base64.StdEncoding.EncodeToString([]byte("file body"))
	resp = do(t, "POST", srv.URL+"/v1/sandboxes/web-1/file", `{"path":"app/main.txt","content":"`+content+`"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("write file status = %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", srv.URL+"/v1/sandboxes/web-1/file", `{"path":"app/main.txt"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read file status = %d, want 200", resp.StatusCode)
	}
	if data, _ := io.ReadAll(resp.Body); string(data) != "file body" {
		t.Fatalf("read file = %q, want %q", data, "file body")
	}

	// Exec.
	resp = do(t, "POST", srv.URL+"/v1/sandboxes/web-1/cmd", `{"command":["sh","-c","cat app/main.txt"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cmd status = %d, want 200", resp.StatusCode)
	}
	res := decode[guest.CmdResult](t, resp)
	if res.Stdout != "file body" || res.ExitCode != 0 {
		t.Fatalf("cmd result = %+v, want stdout %q", res, "file body")
	}

	// Suspend, then resume and exec again.
	resp = do(t, "POST", srv.URL+"/v1/sandboxes/web-1/suspend", "")
	if got := decode[service.SandboxInfo](t, resp); got.Status != "suspended" {
		t.Fatalf("after suspend = %+v, want suspended", got)
	}
	resp = do(t, "POST", srv.URL+"/v1/sandboxes/web-1/resume", "")
	if got := decode[service.SandboxInfo](t, resp); got.Status != "running" {
		t.Fatalf("after resume = %+v, want running", got)
	}
	resp = do(t, "POST", srv.URL+"/v1/sandboxes/web-1/cmd", `{"command":["sh","-c","echo back"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cmd after resume status = %d, want 200", resp.StatusCode)
	}
	if res := decode[guest.CmdResult](t, resp); res.Stdout != "back\n" {
		t.Fatalf("cmd after resume = %+v, want stdout %q", res, "back\n")
	}

	// List directory.
	resp = do(t, "GET", srv.URL+"/v1/sandboxes/web-1/dir", `{"path":"app"}`)
	listing := decode[guest.ListDirResponse](t, resp)
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "main.txt" {
		t.Fatalf("listing = %+v, want [main.txt]", listing.Entries)
	}

	// A file deletes cleanly through the file endpoint.
	resp = do(t, "DELETE", srv.URL+"/v1/sandboxes/web-1/file", `{"path":"app/main.txt"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete file status = %d, want 204", resp.StatusCode)
	}

	// Delete the directory tree.
	resp = do(t, "DELETE", srv.URL+"/v1/sandboxes/web-1/dir", `{"path":"app"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete dir status = %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", srv.URL+"/v1/sandboxes/web-1/dir", `{"path":"app"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("list after delete status = %d, want 404", resp.StatusCode)
	}

	// Delete.
	resp = do(t, "DELETE", srv.URL+"/v1/sandboxes/web-1", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", srv.URL+"/v1/sandboxes/web-1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", resp.StatusCode)
	}
}

func TestCreateStartsSandbox(t *testing.T) {
	srv, router := newAPI(t)
	fsSys, _ := guestsys.New(t.TempDir())
	h, err := (&guest.Server{FS: fsSys}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	router.Register("started", h)

	// Create starts the sandbox.
	resp := do(t, "POST", srv.URL+"/v1/sandboxes", `{"id":"started","template":"default","namespace":"sandboxes"}`)
	if got := decode[service.SandboxInfo](t, resp); got.Status != "running" {
		t.Errorf("create = %+v, want running", got)
	}
}

func TestValidation(t *testing.T) {
	srv, _ := newAPI(t)

	resp := do(t, "POST", srv.URL+"/v1/sandboxes", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create without id status = %d, want 400", resp.StatusCode)
	}
	// Omitting template and namespace falls back to the defaults.
	resp = do(t, "POST", srv.URL+"/v1/sandboxes", `{"id":"bare"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("create with defaults status = %d, want 201", resp.StatusCode)
	}
	got := decode[service.SandboxInfo](t, resp)
	if got.Template != service.DefaultTemplate || got.Namespace != service.DefaultNamespace {
		t.Errorf("template with defaults = %q in %q, want %q in %q",
			got.Template, got.Namespace, service.DefaultTemplate, service.DefaultNamespace)
	}
	resp = do(t, "GET", srv.URL+"/v1/sandboxes/absent", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get absent status = %d, want 404", resp.StatusCode)
	}
	apiErr := decode[guest.Error](t, resp)
	if apiErr.Code != guest.CodeNotFound {
		t.Errorf("error code = %q, want %q", apiErr.Code, guest.CodeNotFound)
	}
}
