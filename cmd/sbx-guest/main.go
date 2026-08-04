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
		addr    = flag.String("addr", "", "address to listen on (defaults to :$PORT, or :80)")
		workdir = flag.String("workdir", "/", "base directory for relative paths and default exec cwd")
	)
	flag.Parse()

	if *addr == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "80"
		}
		*addr = ":" + port
	}

	if err := os.MkdirAll(*workdir, 0o755); err != nil {
		log.Fatalf("creating workdir %s: %v", *workdir, err)
	}

	srv := &guest.Server{Workdir: *workdir}
	log.Printf("sbx-guest listening on %s (workdir %s)", *addr, *workdir)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}
