// Command ate-env-api serves the env API over HTTP and gRPC. It bridges
// clients to the Substrate control plane (ateapi) for actor lifecycle
// and to the atenet router for in-env operations: REST guest calls are
// reverse-proxied, and ateenv.v1alpha1 gRPC calls are forwarded opaquely
// to the guest named in the ate-env-id request metadata.
package main

import (
	"context"
	"flag"
	"log"
	"net"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/grpcmux"
	"github.com/agent-substrate/env/internal/grpcproxy"
	"github.com/agent-substrate/env/internal/service"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc"
)

func main() {
	var (
		listen     = flag.String("listen", "0.0.0.0:7777", "address to serve the API on")
		ateapi     = flag.String("ateapi", ate.DefaultControlAddr, "address of the ateapi gRPC control plane")
		atenet     = flag.String("atenet", ate.DefaultRouterAddr, "address of the atenet HTTP router")
		atenetTLS  = flag.Bool("atenet-tls", false, "dial the atenet router with TLS; pairs with atenet --https-h2 for gRPC guest traffic")
		hostSuffix = flag.String("host-suffix", ate.DefaultHostSuffix, "atenet router host suffix for actor routing")
		skipVerify = flag.Bool("skip-verify", true, "skip TLS certificate verification on the control plane connection")
		tokenFile  = flag.String("ateapi-token-file", "", "file with a bearer token for the control plane, e.g. a projected ServiceAccount token with the ateapi audience")
	)
	flag.Parse()

	client, err := ate.New(ate.Options{
		ControlAddr:     *ateapi,
		RouterAddr:      *atenet,
		RouterTLS:       *atenetTLS,
		HostSuffix:      *hostSuffix,
		SkipVerify:      *skipVerify,
		BearerTokenFile: *tokenFile,
	})
	if err != nil {
		log.Fatalf("creating env client: %v", err)
	}
	defer client.Close()

	grpcServer := grpc.NewServer(
		grpc.ForceServerCodec(grpcproxy.Codec()),
		grpc.UnknownServiceHandler(grpcproxy.StreamHandler(
			func(ctx context.Context, envID string) (*grpc.ClientConn, error) {
				return client.GuestConn(envID)
			},
		)),
	)
	ateenvv1.RegisterEnvironmentServiceServer(grpcServer, apiservice.New(client))

	handler := grpcmux.Handler(grpcServer, service.Handler(client))

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listening on %s: %v", *listen, err)
	}
	defer lis.Close()

	log.Printf("ate-env-api listening on %s (ateapi %s, atenet %s)", lis.Addr(), *ateapi, *atenet)
	log.Fatal(grpcmux.Server(handler).Serve(lis))
}
