// Command sbx-guest is the daemon that runs inside a Substrate actor
// and exposes command execution and filesystem access over HTTP. It is the
// in-sandbox half of the sandbox service; the atenet router forwards
// per-sandbox traffic to it.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/agent-substrate/sandbox/internal/guest"
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
	h, err := srv.Handler()
	if err != nil {
		log.Fatalf("initializing guest server: %v", err)
	}
	log.Printf("sbx-guest listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, h))
}
