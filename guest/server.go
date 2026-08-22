// Package guest provides the in-actor guest daemon server and configuration
// for Agent Substrate environments.
package guest

import (
	"fmt"
	"strings"

	"github.com/agent-substrate/env/guest/filesystem"
	"github.com/agent-substrate/env/guest/process"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

const (
	// DefaultPort is the fallback listen port when PORT is unspecified.
	DefaultPort = "8080"
)

// Config specifies the runtime configuration for the guest daemon.
type Config struct {
	// ListenAddr is the TCP network address to listen on (e.g. ":8080" or ":80").
	ListenAddr string
	// LogDir is the directory where process output logs are spooled.
	LogDir string
	// Workspace is the working and confinement directory for filesystem and process operations.
	Workspace string
	// EnableProcess indicates whether the ProcessService gRPC service is enabled.
	EnableProcess bool
	// EnableFileSystem indicates whether the FileSystemService gRPC service is enabled.
	EnableFileSystem bool
}

// DefaultConfig returns the default configuration with all services enabled.
func DefaultConfig() Config {
	return Config{
		ListenAddr:       ":" + DefaultPort,
		EnableProcess:    true,
		EnableFileSystem: true,
	}
}

// FormatEnabledServices formats the list of active services for logging.
func FormatEnabledServices(cfg Config) string {
	var active []string
	if cfg.EnableProcess {
		active = append(active, "process")
	}
	if cfg.EnableFileSystem {
		active = append(active, "filesystem")
	}
	if len(active) == 0 {
		return "none"
	}
	return strings.Join(active, ",")
}

// NewServer configures and returns the gRPC server and a cleanup function based on cfg.
func NewServer(cfg Config) (*grpc.Server, func(), error) {
	var cleanups []func()

	grpcServer := grpc.NewServer()
	reflection.Register(grpcServer)

	if cfg.EnableProcess {
		tracker, err := process.NewTracker(process.DefaultConfig(cfg.LogDir))
		if err != nil {
			return nil, nil, fmt.Errorf("initializing process tracker: %w", err)
		}
		cleanups = append(cleanups, func() {
			tracker.Close()
		})
		ateenvv1.RegisterProcessServiceServer(grpcServer, process.NewService(tracker))
	}

	if cfg.EnableFileSystem {
		fsSvc := filesystem.NewService(filesystem.Config{RootDirectory: cfg.Workspace})
		ateenvv1.RegisterFileSystemServiceServer(grpcServer, fsSvc)
	}

	cleanup := func() {
		grpcServer.GracefulStop()
		for _, c := range cleanups {
			c()
		}
	}

	return grpcServer, cleanup, nil
}
