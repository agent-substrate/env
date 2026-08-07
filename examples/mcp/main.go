// Command mcp demonstrates connecting to an environment's MCP endpoint using the
// official github.com/modelcontextprotocol/go-sdk package: creating an environment,
// discovering built-in tools over MCP, and executing a tool call.
//
// It expects a port-forward to the ate-env-api service:
//
//	kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/agent-substrate/env/env"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx := context.Background()

	// 1. Create an environment via the env Client.
	c, err := env.NewClient(env.ClientOptions{
		Endpoint: "http://localhost:7777",
	})
	if err != nil {
		log.Fatalf("connecting to Substrate: %v", err)
	}
	defer c.Close()

	env, err := c.Create(ctx, "mcp-demo")
	if err != nil {
		log.Fatalf("creating environment: %v", err)
	}
	defer env.Delete(ctx)

	// 2. Initialize MCP Client using official go-sdk.
	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "mcp-demo-agent",
		Version: "1.0.0",
	}, nil)

	// 3. Connect to the environment's MCP endpoint.
	transport := &mcp.StreamableClientTransport{
		Endpoint: "http://localhost:7777/v1/envs/mcp-demo/mcp",
	}

	session, err := mcpClient.Connect(ctx, transport, nil)
	if err != nil {
		log.Fatalf("connecting to environment MCP endpoint: %v", err)
	}
	defer session.Close()

	// 4. Discover tools.
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		log.Fatalf("listing tools: %v", err)
	}

	fmt.Println("Discovered tools:")
	for _, tool := range tools.Tools {
		fmt.Printf("- %s: %s\n", tool.Name, tool.Description)
	}

	// 5. Execute a tool call (e.g. shell).
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "shell",
		Arguments: map[string]any{
			"command": "uname -a",
		},
	})
	if err != nil {
		log.Fatalf("calling tool: %v", err)
	}

	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			fmt.Println("\nTool output:\n" + text.Text)
		}
	}
}
