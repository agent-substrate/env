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
	listen := flag.String("listen", "", "address to serve the guest API on (defaults to :$PORT, or :80)")
	flag.Parse()

	if *listen == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "80"
		}
		*listen = ":" + port
	}

	srv := &guest.Server{}
	h, err := srv.Handler(nil)
	if err != nil {
		log.Fatalf("initializing guest server: %v", err)
	}

	log.Printf("ate-env-guest listening on %s (serving REST API and /v1/mcp)", *listen)
	log.Fatal(http.ListenAndServe(*listen, h))
}
