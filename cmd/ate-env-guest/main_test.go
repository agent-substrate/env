package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/agent-substrate/env/guest"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestServerHealthzAndGRPC(t *testing.T) {
	tempDir := t.TempDir()
	cfg := guest.Config{
		Workspace:        tempDir,
		LogDir:           tempDir,
		EnableProcess:    true,
		EnableFileSystem: true,
	}

	grpcServer, cleanup, err := guest.NewServer(cfg)
	if err != nil {
		t.Fatalf("failed to initialize guest server: %v", err)
	}
	defer cleanup()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/", grpcServer)

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := &http.Server{
		Handler:   mux,
		Protocols: &protocols,
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	go func() {
		_ = srv.Serve(lis)
	}()
	defer func() {
		grpcServer.GracefulStop()
		_ = srv.Shutdown(context.Background())
	}()

	// 1. Test HTTP GET /readyz over HTTP/1.1
	resp, err := http.Get("http://" + lis.Addr().String() + "/readyz")
	if err != nil {
		t.Fatalf("HTTP GET /readyz failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}
	if string(body) != "ok\n" {
		t.Errorf("body = %q, want %q", string(body), "ok\n")
	}

	// 2. Test gRPC over HTTP/2 cleartext (h2c)
	ctx := context.Background()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial gRPC: %v", err)
	}
	defer conn.Close()

	fsClient := ateenvv1alpha.NewFileSystemServiceClient(conn)
	testFile := filepath.Join(tempDir, "hello.txt")
	writeStream, err := fsClient.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := writeStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  testFile,
		Chunk: []byte("hello world"),
	}); err != nil {
		t.Fatalf("failed to send chunk: %v", err)
	}
	writeRes, err := writeStream.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteFile CloseAndRecv failed: %v", err)
	}
	if writeRes.BytesWritten != int64(len("hello world")) {
		t.Fatalf("expected %d bytes, got %d", len("hello world"), writeRes.BytesWritten)
	}
}
