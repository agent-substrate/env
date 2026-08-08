// Command quickstart demonstrates the environment SDK end to end: create an
// environment, write and run code in it, and read output files back.
//
// It expects a port-forward to the ate-env-api service:
//
//	kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/agent-substrate/env/env"
)

func main() {
	ctx := context.Background()

	client, err := env.NewClient(env.ClientOptions{
		Endpoint: "http://localhost:7777",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	env, err := client.Create(ctx, env.CreateEnvRequest{ID: "quickstart-1"})
	if err != nil {
		log.Fatal(err)
	}
	defer env.Delete(ctx)

	// Write a script into the environment and run it.
	script := "#!/bin/sh\necho \"hello from $(hostname)\"\ndate > /workspace/last-run\n"
	if err := env.WriteFile(ctx, "/workspace/hello.sh", strings.NewReader(script), 0o755); err != nil {
		log.Fatal(err)
	}
	res, err := env.Shell(ctx, "/workspace/hello.sh")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Stdout)

	rc, err := env.ReadFile(ctx, "/workspace/last-run")
	if err != nil {
		log.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("last run at %s", data)
}
