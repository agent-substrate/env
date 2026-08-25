// Command mcp demonstrates connecting to an environment's MCP endpoint using the
// official github.com/modelcontextprotocol/go-sdk package: discovering built-in
// tools over MCP, and executing a tool call.
//
// When running against a cluster with a port-forwarded ate-env-api:
//
//	kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
//	go run ./examples/mcp/main.go
//
// When testing against a local ate-env-api without a control plane:
//
//	go run ./examples/mcp/main.go -skip-create -env=my-env
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/agent-substrate/env/env"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	endpoint := flag.String("endpoint", "http://localhost:7777", "ate-env-api endpoint")
	envID := flag.String("env", "mcp-demo", "environment ID")
	template := flag.String("template", "env-python", "ActorTemplate name")
	skipCreate := flag.Bool("skip-create", false, "skip environment creation via control plane (useful for local testing)")
	flag.Parse()

	ctx := context.Background()

	// 1. Optionally create the environment via the env Client if talking to a cluster.
	if !*skipCreate {
		c, err := env.NewClient(env.ClientOptions{
			Endpoint: *endpoint,
		})
		if err != nil {
			log.Fatalf("connecting to Substrate: %v", err)
		}
		defer c.Close()

		e, err := c.Create(ctx, &ateenvv1.CreateEnvironmentRequest{
			Id:       *envID,
			Template: &ateenvv1.Template{Name: *template},
		})
		if err != nil {
			log.Fatalf("creating environment: %v", err)
		}
		defer e.Delete(ctx)
	}

	// 2. Initialize MCP Client using official go-sdk.
	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "mcp-demo-agent",
		Version: "1.0.0",
	}, nil)

	// 3. Connect to the environment's MCP endpoint.
	mcpURL := fmt.Sprintf("%s/v1/envs/%s/mcp", *endpoint, *envID)
	transport := &mcp.StreamableClientTransport{
		Endpoint: mcpURL,
	}

	session, err := mcpClient.Connect(ctx, transport, nil)
	if err != nil {
		log.Fatalf("connecting to environment MCP endpoint (%s): %v", mcpURL, err)
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

	// 5. Execute a tool call (e.g. shell), retrying briefly for container warmup.
	var result *mcp.CallToolResult
	for attempt := 1; attempt <= 10; attempt++ {
		result, err = session.CallTool(ctx, &mcp.CallToolParams{
			Name: "shell",
			Arguments: map[string]any{
				"command": "uname -a",
			},
		})
		if err == nil && len(result.Content) > 0 {
			if txt, ok := result.Content[0].(*mcp.TextContent); ok && !strings.Contains(txt.Text, "connect error") {
				break
			}
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		log.Fatalf("calling tool: %v", err)
	}

	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			fmt.Println("\nTool output:\n" + text.Text)
		}
	}
}
