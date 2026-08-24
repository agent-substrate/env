// Command ate-env-mcp bridges an agent harness to one environment: it
// speaks MCP over stdio on one side and the ateenv.v1alpha1 gRPC data
// plane against ate-env-api on the other. Point any MCP-capable
// harness at it and the harness gets shell, files, and background
// processes inside the environment — with suspend and resume invisible.
//
// The API endpoint can be set with the -api flag or the
// SUBSTRATE_ENV_API environment variable.
package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/agent-substrate/env/env"
	"github.com/agent-substrate/env/internal/mcpbridge"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	var (
		api    = flag.String("api", envOr("SUBSTRATE_ENV_API", "127.0.0.1:7777"), "address of the ate-env-api service")
		envID  = flag.String("env", "", "environment id to bridge to (required)")
		create = flag.Bool("create", false, "create the environment first (with server defaults)")
	)
	flag.Parse()
	// The harness owns stdout; everything else goes to stderr.
	log.SetOutput(os.Stderr)

	if *envID == "" {
		log.Fatal("ate-env-mcp: -env is required")
	}

	conn, err := grpc.NewClient(*api, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("ate-env-mcp: connecting to %s: %v", *api, err)
	}
	defer conn.Close()

	if *create {
		client, err := env.NewClient(env.ClientOptions{GRPCConn: conn})
		if err != nil {
			log.Fatalf("ate-env-mcp: %v", err)
		}
		if _, err := client.Create(context.Background(), &ateenvv1.CreateEnvironmentRequest{Id: *envID}); err != nil {
			log.Fatalf("ate-env-mcp: creating %q: %v", *envID, err)
		}
		log.Printf("ate-env-mcp: created environment %q", *envID)
	}

	srv := mcpbridge.NewServer(conn, *envID)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("ate-env-mcp: %v", err)
	}
}
