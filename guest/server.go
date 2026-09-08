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

// Package guest provides the in-actor guest daemon server and configuration
// for Agent Substrate environments.
package guest

import (
	"fmt"
	"strings"

	"github.com/agent-substrate/env/guest/filesystem"
	"github.com/agent-substrate/env/guest/process"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
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
		trackerCfg := process.DefaultConfig(cfg.LogDir)
		if cfg.Workspace != "" {
			trackerCfg.Workspace = cfg.Workspace
		}
		tracker, err := process.NewTracker(trackerCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("initializing process tracker: %w", err)
		}
		cleanups = append(cleanups, func() {
			tracker.Close()
		})
		ateenvv1alpha.RegisterProcessServiceServer(grpcServer, process.NewService(tracker))
	}

	if cfg.EnableFileSystem {
		fsSvc := filesystem.NewService(filesystem.Config{RootDirectory: cfg.Workspace})
		ateenvv1alpha.RegisterFileSystemServiceServer(grpcServer, fsSvc)
	}

	cleanup := func() {
		grpcServer.GracefulStop()
		for _, c := range cleanups {
			c()
		}
	}

	return grpcServer, cleanup, nil
}
