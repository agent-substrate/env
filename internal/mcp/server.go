// Package mcp implements an MCP (Model Context Protocol) server using
// github.com/modelcontextprotocol/go-sdk/mcp.
package mcp

import (
	"context"
	"net/http"

	"github.com/agent-substrate/env/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server wraps an mcp.Server backed by a tool.Registry.
type Server struct {
	mcpServer *mcp.Server
	handler   http.Handler
}

// NewServer creates an MCP server using github.com/modelcontextprotocol/go-sdk.
func NewServer(reg *tool.Registry) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{
		Name:    "ate-env-guest",
		Version: "1.0.0",
	}, nil)

	for _, toolObj := range reg.Definitions() {
		mcpSrv.AddTool(toolObj, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return reg.Invoke(ctx, req.Params.Name, req.Params.Arguments), nil
		})
	}

	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return mcpSrv
	}, &mcp.StreamableHTTPOptions{
		JSONResponse:               true,
		Stateless:                  true,
		DisableLocalhostProtection: true,
	})

	return &Server{
		mcpServer: mcpSrv,
		handler:   handler,
	}
}

// MCPServer returns the underlying *mcp.Server.
func (s *Server) MCPServer() *mcp.Server {
	return s.mcpServer
}

// ServeHTTP handles MCP HTTP transport requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}
