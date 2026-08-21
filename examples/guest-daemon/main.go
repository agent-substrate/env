// Command main runs an example guest daemon for Agent Substrate environments,
// demonstrating how to configure and run the guest daemon using the guest package.
//
// For the primary daemon binary deployed into Substrate environments, see cmd/ate-env-guest.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/agent-substrate/env/guest"
)

func main() {
	listen := flag.String("listen", "", "address to listen on (defaults to :$PORT, or :8080)")
	logDir := flag.String("log-dir", "", "directory for process logs (defaults to $LOG_DIR or /var/log/ate-jobs)")
	workspace := flag.String("workspace", "", "workspace directory for container operations (defaults to $WORKSPACE, $WORKDIR, or cwd)")
	enableProcess := flag.Bool("enable-process", true, "enable ProcessService gRPC service")
	enableFileSystem := flag.Bool("enable-filesystem", true, "enable FileSystemService gRPC service")
	flag.Parse()

	addr := *listen
	if addr == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = guest.DefaultPort
		}
		if !strings.Contains(port, ":") {
			addr = ":" + port
		} else {
			addr = port
		}
	} else if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}

	resolvedLogDir := *logDir
	if resolvedLogDir == "" {
		resolvedLogDir = os.Getenv("LOG_DIR")
	}

	resolvedWorkspace := *workspace
	if resolvedWorkspace == "" {
		if envWork := os.Getenv("WORKSPACE"); envWork != "" {
			resolvedWorkspace = envWork
		} else if envWork := os.Getenv("WORKDIR"); envWork != "" {
			resolvedWorkspace = envWork
		} else if cwd, err := os.Getwd(); err == nil {
			resolvedWorkspace = cwd
		}
	}

	cfg := guest.Config{
		ListenAddr:       addr,
		LogDir:           resolvedLogDir,
		Workspace:        resolvedWorkspace,
		EnableProcess:    *enableProcess,
		EnableFileSystem: *enableFileSystem,
	}

	grpcServer, cleanup, err := guest.NewServer(cfg)
	if err != nil {
		log.Fatalf("failed to initialize guest daemon: %v", err)
	}
	defer cleanup()

	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", cfg.ListenAddr, err)
	}
	defer lis.Close()

	// Handle graceful shutdown on SIGINT / SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		fmt.Printf("\nReceived signal %v, shutting down guest daemon gracefully...\n", sig)
		grpcServer.GracefulStop()
		_ = lis.Close()
		cleanup()
	}()

	log.Printf("guest-daemon listening on gRPC %s (services=%s, workspace=%s, logdir=%s)",
		lis.Addr(), guest.FormatEnabledServices(cfg), cfg.Workspace, cfg.LogDir)
	if err := grpcServer.Serve(lis); err != nil && err != net.ErrClosed {
		log.Fatalf("guest-daemon server terminated: %v", err)
	}
}
