// Command ate-env-api serves the env API. It bridges HTTP
// clients to the Substrate control plane (ateapi) for actor lifecycle
// and to the atenet router for in-env exec and filesystem operations.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/service"
)

func main() {
	var (
		listen     = flag.String("listen", "0.0.0.0:7777", "address to serve the API on")
		ateapi     = flag.String("ateapi", ate.DefaultControlAddr, "address of the ateapi gRPC control plane")
		atenet     = flag.String("atenet", ate.DefaultRouterAddr, "address of the atenet HTTP router")
		hostSuffix = flag.String("host-suffix", ate.DefaultHostSuffix, "atenet router host suffix for actor routing")
		skipVerify = flag.Bool("skip-verify", true, "skip TLS certificate verification on the control plane connection")
		tokenFile  = flag.String("ateapi-token-file", "", "file with a bearer token for the control plane, e.g. a projected ServiceAccount token with the ateapi audience")
	)
	flag.Parse()

	client, err := ate.New(ate.Options{
		ControlAddr:     *ateapi,
		RouterAddr:      *atenet,
		HostSuffix:      *hostSuffix,
		SkipVerify:      *skipVerify,
		BearerTokenFile: *tokenFile,
	})
	if err != nil {
		log.Fatalf("creating env client: %v", err)
	}
	defer client.Close()

	log.Printf("ate-env-api listening on %s (ateapi %s, atenet %s)", *listen, *ateapi, *atenet)
	log.Fatal(http.ListenAndServe(*listen, service.Handler(client)))
}
