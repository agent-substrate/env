package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
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

	// Guest-bound gRPC (ProcessService, FileSystemService, reflection —
	// any service NOT registered here) is forwarded opaquely to the
	// environment named in x-env-id metadata: the pass-through codec plus
	// UnknownServiceHandler relay raw frames over one cached connection
	// per environment, so the guest contract can evolve without touching
	// this binary.
	//
	// SEE(lior): this replaces #35's typed per-method forwarding in
	// apiservice, which needed an api-side method (and a new deploy) for
	// every guest RPC and dialed a fresh connection per call.
	grpcServer := grpc.NewServer(
		grpc.ForceServerCodec(grpcproxy.Codec()),
		grpc.UnknownServiceHandler(grpcproxy.StreamHandler(
			func(ctx context.Context, atespace, envID string) (*grpc.ClientConn, error) {
				return client.GuestConn(atespace, envID)
			},
		)),
	)
	ateenvv1.RegisterEnvironmentServiceServer(grpcServer, apiservice.New(client))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/v1/", service.Handler(client))
	mux.Handle("/", grpcServer)

	// Enable both HTTP/1.1 (for /healthz and /v1/ HTTP endpoints) and
	// unencrypted HTTP/2 (h2c, for gRPC services) on the same plaintext TCP listener.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := &http.Server{
		Addr:      *listen,
		Handler:   mux,
		Protocols: &protocols,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("ate-env-api listening on %s (ateapi %s, atenet %s)", *listen, *ateapi, *atenet)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}
