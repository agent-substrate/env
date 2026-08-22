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
