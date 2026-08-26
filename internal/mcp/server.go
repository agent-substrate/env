// Package mcp implements an MCP (Model Context Protocol) server using
// github.com/modelcontextprotocol/go-sdk/mcp, backed by FileSystemService
// and ProcessService gRPC clients.
package mcp

import (
	"context"
	"net/http"
	"sync"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/tool"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server wraps an mcp.Server backed by a tool.Registry.
type Server struct {
	mcpServer *mcp.Server
}

// NewServer creates an MCP server using github.com/modelcontextprotocol/go-sdk.
func NewServer(reg *tool.Registry) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{
		Name:    "ate-env-api",
		Version: "1.0.0",
	}, nil)

	for _, t := range reg.Definitions() {
		mcpSrv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	return NewServer(reg)
}

// MCPServer returns the underlying mcp.Server instance.
func (s *Server) MCPServer() *mcp.Server {
	return s.mcpServer
}

// NewHandler returns an http.Handler that dynamically serves MCP requests for
// environment actors backed by client.
func NewHandler(client *ate.Client) http.Handler {
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
		fsClient := ateenvv1.NewFileSystemServiceClient(conn)
		procClient := ateenvv1.NewProcessServiceClient(conn)
		srv := NewServerForClients(fsClient, procClient)
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
