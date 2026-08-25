// Command ate-env-guest is the daemon that runs inside a Substrate actor
// and exposes command execution and filesystem access over gRPC
// (ProcessService and FileSystemService) with HTTP readiness probe support.
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

	"github.com/agent-substrate/env/guest"
)

func main() {
	listen := flag.String("listen", ":80", "address to serve the guest API on")
	logDir := flag.String("log-dir", "/var/log/ate-jobs", "directory for process logs")
	workspace := flag.String("workspace", "/", "workspace root directory")
	flag.Parse()

	addr := *listen
	if addr == "" {
		addr = ":80"
	}

	cfg := guest.Config{
		ListenAddr:       addr,
		LogDir:           *logDir,
		Workspace:        *workspace,
		EnableProcess:    true,
		EnableFileSystem: true,
	}

	grpcServer, cleanup, err := guest.NewServer(cfg)
	if err != nil {
		log.Fatalf("failed to initialize guest server: %v", err)
	}
	defer cleanup()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/", grpcServer)

	// Enable both HTTP/1.1 (for the /readyz probe) and unencrypted HTTP/2
	// (h2c, for gRPC services) on the same plaintext TCP listener.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := &http.Server{
		Handler:   mux,
		Protocols: &protocols,
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}
	defer lis.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Printf("\nShutting down ate-env-guest...")
		grpcServer.GracefulStop()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("ate-env-guest listening on %s (gRPC services=%s, workspace=%s, logdir=%s)",
		lis.Addr(), guest.FormatEnabledServices(cfg), cfg.Workspace, cfg.LogDir)
	if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("ate-env-guest server error: %v", err)
	}
}
