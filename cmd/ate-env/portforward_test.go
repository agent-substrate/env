package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetRESTConfigExplicitMissingPath(t *testing.T) {
	_, err := getRESTConfig("/non/existent/kubeconfig/path", "")
	if err == nil {
		t.Error("expected error for non-existent kubeconfig path, got nil")
	}
}

func TestGetRESTConfigInvalidContext(t *testing.T) {
	tempDir := t.TempDir()
	kubeconfigPath := filepath.Join(tempDir, "config")
	content := `apiVersion: v1
kind: Config
clusters: []
contexts: []
users: []
`
	if err := os.WriteFile(kubeconfigPath, []byte(content), 0o600); err != nil {
		t.Fatalf("writing temp kubeconfig: %v", err)
	}

	_, err := getRESTConfig(kubeconfigPath, "non-existent-context")
	if err == nil {
		t.Error("expected error for non-existent context, got nil")
	}
}
