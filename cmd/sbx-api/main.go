// Command sbx-api serves the sandbox API. It bridges HTTP
// clients to the Substrate control plane (ateapi) for sandbox lifecycle
// and to the atenet router for in-sandbox exec and filesystem operations.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/agent-substrate/sandbox/internal/ate"
	"github.com/agent-substrate/sandbox/internal/service"
)

func main() {
	var (
		listen     = flag.String("listen", "0.0.0.0:7777", "address to serve the API on")
		ateapi     = flag.String("ateapi", ate.DefaultControlAddr, "address of the ateapi gRPC control plane")
		atenet     = flag.String("atenet", ate.DefaultRouterAddr, "address of the atenet HTTP router")
		hostSuffix = flag.String("host-suffix", ate.DefaultHostSuffix, "atenet router host suffix for actor routing")
		skipVerify = flag.Bool("skip-verify", true, "skip TLS certificate verification on the control plane connection")
	)
	flag.Parse()

	client, err := ate.New(ate.Options{
		ControlAddr: *ateapi,
		RouterAddr:  *atenet,
		HostSuffix:  *hostSuffix,
		SkipVerify:  *skipVerify,
	})
	if err != nil {
		log.Fatalf("creating sandbox client: %v", err)
	}
	defer client.Close()

	log.Printf("sbx-api listening on %s (ateapi %s, atenet %s)", *listen, *ateapi, *atenet)
	log.Fatal(http.ListenAndServe(*listen, service.Handler(client)))
}
