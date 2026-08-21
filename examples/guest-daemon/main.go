// Command main runs a reference guest daemon for Agent Substrate environments,
// assembling ProcessService and FileSystemService onto a gRPC server.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/agent-substrate/env/guestd/filesystem"
	"github.com/agent-substrate/env/guestd/process"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {
	listen := flag.String("listen", "", "address to listen on (defaults to :$PORT, or :8080)")
	logDir := flag.String("log-dir", "", "directory for process logs (defaults to $LOG_DIR or /var/log/ate-jobs)")
	workDir := flag.String("workdir", "", "working directory for container operations (defaults to $WORKDIR or cwd)")
	flag.Parse()

	if *listen == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		*listen = ":" + port
	}

	if *workDir == "" {
		if envWorkDir := os.Getenv("WORKDIR"); envWorkDir != "" {
			*workDir = envWorkDir
		} else if cwd, err := os.Getwd(); err == nil {
			*workDir = cwd
		}
	}

	tracker, err := process.NewTracker(process.DefaultConfig(*logDir))
	if err != nil {
		log.Fatalf("failed to initialize process tracker: %v", err)
	}
	defer tracker.Close()

	grpcServer := grpc.NewServer()
	ateenvv1.RegisterProcessServiceServer(grpcServer, process.NewService(tracker))
	ateenvv1.RegisterFileSystemServiceServer(grpcServer, filesystem.NewService(filesystem.Config{RootDirectory: *workDir}))

	// Enable gRPC reflection for easy testing with grpcurl
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", *listen, err)
	}

	// Handle graceful shutdown on SIGINT / SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		fmt.Printf("\nReceived signal %v, shutting down guest daemon gracefully...\n", sig)
		grpcServer.GracefulStop()
		_ = lis.Close()
	}()

	log.Printf("guest-daemon listening on gRPC %s (workdir=%s, logdir=%s)", *listen, *workDir, *logDir)
	if err := grpcServer.Serve(lis); err != nil && err != net.ErrClosed {
		log.Fatalf("guest-daemon server terminated: %v", err)
	}
}
