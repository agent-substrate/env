// Package mcp implements an MCP (Model Context Protocol) server using
// github.com/modelcontextprotocol/go-sdk/mcp, backed by FileSystemService
// and ProcessService gRPC clients.
package mcp

import (
	"context"
	"net/http"
	"sync"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/idle"
	"github.com/agent-substrate/env/internal/tool"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server wraps an mcp.Server backed by a tool.Registry.
type Server struct {
	mcpServer *mcp.Server
}

// NewServer creates an MCP server using github.com/modelcontextprotocol/go-sdk.
// pin, if non-nil, is invoked when a tool call starts; the func it returns is
// invoked when the call completes. It marks environment activity for idle
// tracking, keeping the environment pinned for the duration of the call.
func NewServer(reg *tool.Registry, pin func() func()) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{
		Name:    "ate-env-api",
		Version: "1.0.0",
	}, nil)

	for _, t := range reg.Definitions() {
		mcpSrv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if pin != nil {
				defer pin()()
			}
			return reg.Invoke(ctx, req.Params.Name, req.Params.Arguments), nil
		})
	}

	return &Server{
		mcpServer: mcpSrv,
	}
}

// NewServerForClients creates an MCP server configured with tools backed by
// the provided FileSystemService and ProcessService gRPC clients.
func NewServerForClients(fsClient ateenvv1.FileSystemServiceClient, procClient ateenvv1.ProcessServiceClient) *Server {
	tools := NewTools(fsClient, procClient)
	reg := tool.NewRegistry()
	_ = reg.Register(tools...)
	return NewServer(reg, nil)
}

// MCPServer returns the underlying mcp.Server instance.
func (s *Server) MCPServer() *mcp.Server {
	return s.mcpServer
}

// HandlerOption configures NewHandler.
type HandlerOption func(*handlerOptions)

type handlerOptions struct {
	tracker *idle.Tracker
}

// WithIdleTracker records MCP tool-call activity on tracker, so the idle
// reaper never suspends an environment with a tool call in flight.
func WithIdleTracker(t *idle.Tracker) HandlerOption {
	return func(o *handlerOptions) { o.tracker = t }
}

// NewHandler returns an http.Handler that dynamically serves MCP requests for
// environment actors backed by client.
func NewHandler(client *ate.Client, opts ...HandlerOption) http.Handler {
	var options handlerOptions
	for _, opt := range opts {
		opt(&options)
	}

	var (
		mu      sync.Mutex
		servers = make(map[string]*Server)
	)

	getServer := func(atespace, id string) (*mcp.Server, error) {
		key := atespace + "/" + id
		mu.Lock()
		defer mu.Unlock()
		if s, ok := servers[key]; ok {
			return s.mcpServer, nil
		}
		conn, err := client.DialGuest(atespace, id)
		if err != nil {
			return nil, err
		}
		reg := tool.NewRegistry()
		if err := reg.Register(NewTools(ateenvv1.NewFileSystemServiceClient(conn), ateenvv1.NewProcessServiceClient(conn))...); err != nil {
			return nil, err
		}
		var pin func() func()
		if options.tracker != nil {
			env := idle.Env{Atespace: atespace, ID: id}
			pin = func() func() { return options.tracker.Pin(env) }
		}
		srv := NewServer(reg, pin)
		servers[key] = srv
		return srv.mcpServer, nil
	}

	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		id := r.PathValue("id")
		if id == "" {
			return nil
		}
		atespace := r.URL.Query().Get("atespace")
		if atespace == "" {
			atespace = ate.DefaultAtespace
		}
		s, err := getServer(atespace, id)
		if err != nil {
			return nil
		}
		return s
	}, &mcp.StreamableHTTPOptions{
		JSONResponse:               true,
		Stateless:                  true,
		DisableLocalhostProtection: true,
	})
}
