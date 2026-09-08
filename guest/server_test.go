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

package guest

import (
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if !cfg.EnableProcess || !cfg.EnableFileSystem {
		t.Errorf("expected default services to be enabled, got %+v", cfg)
	}
}

func TestFormatEnabledServices(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "all enabled",
			cfg:  Config{EnableProcess: true, EnableFileSystem: true},
			want: "process,filesystem",
		},
		{
			name: "process only",
			cfg:  Config{EnableProcess: true, EnableFileSystem: false},
			want: "process",
		},
		{
			name: "filesystem only",
			cfg:  Config{EnableProcess: false, EnableFileSystem: true},
			want: "filesystem",
		},
		{
			name: "none enabled",
			cfg:  Config{EnableProcess: false, EnableFileSystem: false},
			want: "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatEnabledServices(tt.cfg); got != tt.want {
				t.Errorf("FormatEnabledServices() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewServer(t *testing.T) {
	tempDir := t.TempDir()
	logDir := filepath.Join(tempDir, "logs")

	cfg := Config{
		LogDir:           logDir,
		Workspace:        tempDir,
		EnableProcess:    true,
		EnableFileSystem: true,
	}

	server, cleanup, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer cleanup()

	if server == nil {
		t.Fatalf("expected non-nil server")
	}
}
