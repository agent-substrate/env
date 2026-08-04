// Command quickstart demonstrates the sandbox SDK end to end: create a
// sandbox, write and run code in it, suspend it, then resume and observe
// that its filesystem survived the hibernation cycle.
//
// It expects a port-forward to the substrate-sandbox service:
//
//	kubectl port-forward svc/substrate-sandbox 7777:7777
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/agent-substrate/sandbox/sandbox"
)

func main() {
	ctx := context.Background()

	client, err := sandbox.NewClient(sandbox.ClientOptions{
		Endpoint: "http://localhost:7777",
		Template: "sandbox",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	sb, err := client.Create(ctx, "quickstart-1")
	if err != nil {
		log.Fatal(err)
	}
	defer sb.Delete(ctx)

	// Write a script into the sandbox and run it.
	script := "#!/bin/sh\necho \"hello from $(hostname)\"\ndate > /workspace/last-run\n"
	if err := sb.WriteFile(ctx, "/workspace/hello.sh", strings.NewReader(script), 0o755); err != nil {
		log.Fatal(err)
	}
	res, err := sb.Cmd(ctx, "/workspace/hello.sh")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Stdout)

	// Suspend: full memory + filesystem snapshot, worker freed.
	if err := sb.Suspend(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Println("suspended.")

	// Resume and verify the filesystem survived.
	if err := sb.Resume(ctx); err != nil {
		log.Fatal(err)
	}
	rc, err := sb.ReadFile(ctx, "/workspace/last-run")
	if err != nil {
		log.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("resumed; state survived: last run at %s", data)
}
