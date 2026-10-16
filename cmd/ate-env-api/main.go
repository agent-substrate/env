// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/mcp"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
)

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func main() {
	var (
		listen            = flag.String("listen", "127.0.0.1:7777", "address to serve the API on")
		ateapi            = flag.String("ateapi", ate.DefaultControlAddr, "address of the ateapi gRPC control plane")
		atenet            = flag.String("atenet", ate.DefaultRouterAddr, "address of the atenet HTTP router")
		hostSuffix        = flag.String("host-suffix", ate.DefaultHostSuffix, "atenet router host suffix for actor routing")
		skipVerify        = flag.Bool("skip-verify", true, "skip TLS certificate verification on the control plane connection")
		tokenFile         = flag.String("ateapi-token-file", "", "file with a bearer token for the control plane, e.g. a projected ServiceAccount token with the ateapi audience")
		authTokenFile     = flag.String("auth-token-file", "", "file with a bearer token required from API and MCP callers; re-read on each request")
		wakeBudget        = flag.Duration("wake-budget", apiservice.DefaultWakeBudget, "how long a guest call waits for a suspended environment to wake before failing; cold image pulls and cross-region snapshot reads exceed the router's own parking budget")
		wakeRunningBudget = flag.Duration("wake-running-budget", apiservice.DefaultWakeRunningBudget, "how long a guest call waits when the environment is already RUNNING but the router still answers Unavailable (guest not yet listening vs. path down)")
	)
	flag.Parse()

	var callerAuth *apiservice.BearerAuth
	if *authTokenFile != "" {
		var err error
		callerAuth, err = apiservice.NewBearerAuthFromFile(*authTokenFile)
		if err != nil {
			log.Fatalf("loading --auth-token-file: %v", err)
		}
	} else if !isLoopbackAddr(*listen) {
		log.Printf("WARNING: listening on %s without --auth-token-file; all gRPC and MCP callers are unauthenticated", *listen)
	}

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

	// Create the default atespace up front so ActorTemplates can be registered
	// before the first environment is created (templates live in an atespace).
	// Bounded so a hung control plane fails the pod instead of leaving it
	// unlistening; the restart backoff retries until Substrate answers.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := client.EnsureAtespace(ctx, ate.DefaultAtespace); err != nil {
		log.Fatalf("ensuring default atespace %q: %v", ate.DefaultAtespace, err)
	}
	cancel()
	log.Printf("default atespace %q ready", ate.DefaultAtespace)

	apisvc := apiservice.New(client, *atenet, *hostSuffix)
	apisvc.WakeBudget = *wakeBudget
	apisvc.WakeRunningBudget = *wakeRunningBudget
	defer apisvc.Close()

	var grpcOpts []grpc.ServerOption
	if callerAuth != nil {
		grpcOpts = append(grpcOpts,
			grpc.UnaryInterceptor(callerAuth.UnaryInterceptor()),
			grpc.StreamInterceptor(callerAuth.StreamInterceptor()),
		)
	}
	grpcServer := grpc.NewServer(grpcOpts...)
	ateenvv1alpha.RegisterEnvironmentServiceServer(grpcServer, apisvc)
	ateenvv1alpha.RegisterProcessServiceServer(grpcServer, apisvc)
	ateenvv1alpha.RegisterFileSystemServiceServer(grpcServer, apisvc)

	mcpHandler := mcp.NewHandler(client)
	if callerAuth != nil {
		mcpHandler = callerAuth.WrapHTTP(mcpHandler)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/v1alpha/envs/{id}/mcp", mcpHandler)
	mux.Handle("/", grpcServer)

	// Enable both HTTP/1.1 (for /healthz and /v1alpha/ HTTP endpoints) and
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
