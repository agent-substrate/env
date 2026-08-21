// Command ate-env-api serves the env API over HTTP and gRPC. It bridges
// clients to the Substrate control plane (ateapi) for actor lifecycle
// and to the atenet router for in-env exec and filesystem operations.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/service"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
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

	grpcServer := grpc.NewServer()
	ateenvv1.RegisterEnvironmentServiceServer(grpcServer, apiservice.New(client))

	httpHandler := service.Handler(client)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		httpHandler.ServeHTTP(w, r)
	})

	h2cHandler := h2c.NewHandler(handler, &http2.Server{})

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listening on %s: %v", *listen, err)
	}
	defer lis.Close()

	log.Printf("ate-env-api listening on %s (ateapi %s, atenet %s)", lis.Addr(), *ateapi, *atenet)
	log.Fatal(http.Serve(lis, h2cHandler))
}
