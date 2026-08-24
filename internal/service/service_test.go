package service_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/ate"
	guestsys "github.com/agent-substrate/env/internal/service/guestsys"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/mcp"
	"github.com/agent-substrate/env/internal/service"
	"github.com/agent-substrate/env/internal/tool"
	fstool "github.com/agent-substrate/env/internal/tool/fs"
	"github.com/agent-substrate/env/internal/tool/shell"
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

func TestGuestMCPProxy(t *testing.T) {
	srv, router, _, client := newAPI(t)
	t.Chdir(t.TempDir())
	sys := guestsys.New()
	reg := tool.NewRegistry()
	if err := reg.Register(fstool.New(sys, fstool.Config{})...); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(shell.New(sys, shell.Config{})); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mcpSrv := mcp.NewServer(reg)
	mux.HandleFunc("POST /v1/mcp", mcpSrv.ServeHTTP)
	router.Register("web-1", mux)

	// Create environment via direct client.
	if err := client.Create(t.Context(), ate.CreateOptions{
		ID:        "web-1",
		Template:  "default-env",
		Namespace: "envs",
	}); err != nil {
		t.Fatalf("client.Create: %v", err)
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
