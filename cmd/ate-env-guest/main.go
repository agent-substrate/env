// Command ate-env-guest is the daemon that runs inside a Substrate actor
// and exposes command execution, filesystem access, and MCP tools over HTTP
// using github.com/modelcontextprotocol/go-sdk.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/agent-substrate/env/internal/guest"
)

func main() {
	var (
		addr = flag.String("addr", "", "address to listen on (defaults to :$PORT, or :80)")
	)
	flag.Parse()

	if *addr == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "80"
		}
		*addr = ":" + port
	}

	srv := &guest.Server{}
	h, err := srv.Handler(nil)
	if err != nil {
		log.Fatalf("initializing guest server: %v", err)
	}

	log.Printf("ate-env-guest listening on %s (serving REST API and /mcp)", *addr)
	log.Fatal(http.ListenAndServe(*addr, h))
}
