// Command ate-env-guest is the daemon that runs inside a Substrate
// actor. It serves the environment's data planes on one port: the
// ateenv.v1alpha1 gRPC services (processes and filesystem) over h2c,
// and the REST API and MCP tools over HTTP/1.1.
package main

import (
	"flag"
	"log"
	"net"
	"os"

	"github.com/agent-substrate/env/internal/grpcmux"
	"github.com/agent-substrate/env/internal/guest"
	"github.com/agent-substrate/env/internal/guest/guestrpc"
	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/guest/proc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
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

	sys := guestsys.New()

	rest, err := (&guest.Server{}).Handler(sys)
	if err != nil {
		log.Fatalf("initializing guest server: %v", err)
	}

	grpcServer := grpc.NewServer()
	guestrpc.Register(grpcServer, sys, proc.NewTable())
	// Reflection lets grpcurl and similar tools discover the services
	// through the proxy.
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listening on %s: %v", *listen, err)
	}

	log.Printf("ate-env-guest listening on %s (gRPC data plane, REST API, /v1/mcp)", lis.Addr())
	log.Fatal(grpcmux.Server(grpcmux.Handler(grpcServer, rest)).Serve(lis))
}
