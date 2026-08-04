// Command sbx-api serves the sandbox API. It bridges HTTP
// clients to the Substrate control plane (ateapi) for sandbox lifecycle
// and to the atenet router for in-sandbox exec and filesystem operations.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/agent-substrate/sandbox/internal/direct"
	"github.com/agent-substrate/sandbox/internal/service"
)

func main() {
	var (
		listen     = flag.String("listen", "0.0.0.0:7777", "address to serve the API on")
		ateapi     = flag.String("ateapi", direct.DefaultControlAddr, "address of the ateapi gRPC control plane")
		atenet     = flag.String("atenet", direct.DefaultRouterAddr, "address of the atenet HTTP router")
		hostSuffix = flag.String("host-suffix", direct.DefaultHostSuffix, "atenet router host suffix for actor routing")
		skipVerify = flag.Bool("skip-verify", true, "skip TLS certificate verification on the control plane connection")
		autoResume = flag.Bool("auto-resume", true, "resume suspended sandboxes on exec/file operations")
	)
	flag.Parse()

	client, err := direct.New(direct.Options{
		ControlAddr: *ateapi,
		RouterAddr:  *atenet,
		HostSuffix:  *hostSuffix,
		SkipVerify:  *skipVerify,
		AutoResume:  *autoResume,
	})
	if err != nil {
		log.Fatalf("creating sandbox client: %v", err)
	}
	defer client.Close()

	log.Printf("sbx-api listening on %s (ateapi %s, atenet %s)", *listen, *ateapi, *atenet)
	log.Fatal(http.ListenAndServe(*listen, service.Handler(client)))
}
