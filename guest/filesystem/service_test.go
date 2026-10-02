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

package filesystem

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func setupTestFileSystemServer(t *testing.T, configs ...Config) (ateenvv1alpha.FileSystemServiceClient, func()) {
	t.Helper()

	cfg := Config{
		RootDirectory:  "/", // unconfined for generic tests
		ReadBufferSize: 4 * 1024,
	}
	if len(configs) > 0 {
		cfg = configs[0]
	}

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	svc := NewService(cfg)
	ateenvv1alpha.RegisterFileSystemServiceServer(server, svc)

	go func() {
		_ = server.Serve(lis)
	}()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}

	client := ateenvv1alpha.NewFileSystemServiceClient(conn)

	cleanup := func() {
		conn.Close()
		server.Stop()
		lis.Close()
	}

	return client, cleanup
}

func TestWriteAndReadFileSmall(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir, ReadBufferSize: 4 * 1024})
	defer cleanup()

	ctx := context.Background()
	targetPath := filepath.Join(tempDir, "small.txt")
	testData := []byte("Hello, Substrate streaming filesystem!")

	// 1. Write file via client stream
	writeStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	err = writeStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  targetPath,
		Chunk: testData,
		Mode:  0644,
	})
	if err != nil {
		t.Fatalf("failed to send chunk: %v", err)
	}

	writeRes, err := writeStream.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteFile CloseAndRecv failed: %v", err)
	}

	if writeRes.BytesWritten != int64(len(testData)) {
		t.Fatalf("expected %d bytes written, got %d", len(testData), writeRes.BytesWritten)
	}

	// 2. Read file via server stream
	readStream, err := client.ReadFile(ctx, &ateenvv1alpha.ReadFileRequest{
		Path: targetPath,
	})
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var readBuffer bytes.Buffer
	for {
		chunk, err := readStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadFile recv error: %v", err)
		}
		readBuffer.Write(chunk.Data)
	}

	if !bytes.Equal(readBuffer.Bytes(), testData) {
		t.Fatalf("read content mismatch: expected %q, got %q", string(testData), readBuffer.String())
	}
}

func TestWriteAndReadFileMultiChunk(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir, ReadBufferSize: 4 * 1024})
	defer cleanup()

	ctx := context.Background()
	targetPath := filepath.Join(tempDir, "nested", "sub", "large_binary.bin")

	// Generate 128 KB of random data (spanning multiple 4KB chunks)
	dataSize := 128 * 1024
	largeData := make([]byte, dataSize)
	_, _ = rand.Read(largeData)

	// Stream write in 8 KB client chunks
	writeStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	chunkSize := 8 * 1024
	first := true
	for i := 0; i < len(largeData); i += chunkSize {
		end := i + chunkSize
		if end > len(largeData) {
			end = len(largeData)
		}

		req := &ateenvv1alpha.WriteFileRequest{
			Chunk: largeData[i:end],
		}
		if first {
			req.Path = targetPath
			req.Mode = 0755
			first = false
		}

		if err := writeStream.Send(req); err != nil {
			t.Fatalf("failed to send chunk at %d: %v", i, err)
		}
	}

	writeRes, err := writeStream.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteFile CloseAndRecv error: %v", err)
	}
	if writeRes.BytesWritten != int64(dataSize) {
		t.Fatalf("expected %d bytes written, got %d", dataSize, writeRes.BytesWritten)
	}

	// Verify file mode permissions on disk
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("failed to stat written file: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("expected permissions 0755, got %v", info.Mode().Perm())
	}

	// Stream read back and compare
	readStream, err := client.ReadFile(ctx, &ateenvv1alpha.ReadFileRequest{
		Path: targetPath,
	})
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var readBuffer bytes.Buffer
	for {
		chunk, err := readStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read stream error: %v", err)
		}
		readBuffer.Write(chunk.Data)
	}

	if !bytes.Equal(readBuffer.Bytes(), largeData) {
		t.Fatalf("read content does not match original binary data")
	}
}

func TestSandboxConfinementAndTraversal(t *testing.T) {
	tempDir := t.TempDir()
	sandboxRoot := filepath.Join(tempDir, "workspace")
	if err := os.MkdirAll(sandboxRoot, 0755); err != nil {
		t.Fatalf("failed to create sandbox root: %v", err)
	}

	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: sandboxRoot})
	defer cleanup()

	ctx := context.Background()

	// 1. Attempt to write outside sandbox root (absolute path escape)
	outsidePath := filepath.Join(tempDir, "outside.txt")
	writeStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile init failed: %v", err)
	}
	_ = writeStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  outsidePath,
		Chunk: []byte("malicious write"),
	})
	_, err = writeStream.CloseAndRecv()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for path outside sandbox, got %v", err)
	}

	// 2. Attempt path traversal escape (../..)
	traversalPath := filepath.Join(sandboxRoot, "..", "escape.txt")
	writeStream2, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile init failed: %v", err)
	}
	_ = writeStream2.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  traversalPath,
		Chunk: []byte("traversal write"),
	})
	_, err = writeStream2.CloseAndRecv()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for path traversal, got %v", err)
	}

	// 3. Attempt read outside sandbox
	readStream, err := client.ReadFile(ctx, &ateenvv1alpha.ReadFileRequest{
		Path: "/etc/passwd",
	})
	if err != nil {
		t.Fatalf("ReadFile call failed: %v", err)
	}
	_, err = readStream.Recv()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for /etc/passwd, got %v", err)
	}

	// 4. Relative path should successfully stay inside sandboxRoot
	writeStream3, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile init failed: %v", err)
	}
	_ = writeStream3.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  "relative_file.txt",
		Chunk: []byte("valid sandboxed write"),
	})
	res, err := writeStream3.CloseAndRecv()
	if err != nil {
		t.Fatalf("valid sandboxed write failed: %v", err)
	}
	if res.BytesWritten != int64(len("valid sandboxed write")) {
		t.Fatalf("expected written bytes to match")
	}

	// Verify file was written inside sandboxRoot
	expectedLocation := filepath.Join(sandboxRoot, "relative_file.txt")
	if _, err := os.Stat(expectedLocation); err != nil {
		t.Fatalf("file was not created at expected location %s: %v", expectedLocation, err)
	}
}

func TestReadFileNotFound(t *testing.T) {
	client, cleanup := setupTestFileSystemServer(t)
	defer cleanup()

	ctx := context.Background()
	stream, err := client.ReadFile(ctx, &ateenvv1alpha.ReadFileRequest{
		Path: "/non/existent/path/for/sure.txt",
	})
	if err != nil {
		t.Fatalf("unexpected RPC error on call: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestReadFileEmptyPath(t *testing.T) {
	client, cleanup := setupTestFileSystemServer(t)
	defer cleanup()

	ctx := context.Background()
	stream, err := client.ReadFile(ctx, &ateenvv1alpha.ReadFileRequest{
		Path: "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestWriteFileMissingPath(t *testing.T) {
	client, cleanup := setupTestFileSystemServer(t)
	defer cleanup()

	ctx := context.Background()
	writeStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Send chunk with missing path on first message
	_ = writeStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  "",
		Chunk: []byte("orphan chunk"),
	})

	_, err = writeStream.CloseAndRecv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing path, got %v", err)
	}
}

func TestAppendPreservesExistingContent(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir, ReadBufferSize: 4 * 1024})
	defer cleanup()

	ctx := context.Background()
	targetPath := filepath.Join(tempDir, "transcript.jsonl")
	initial := []byte("{\"seq\":0}\n")
	second := []byte("{\"seq\":1}\n")
	third := []byte("{\"seq\":2}\n")

	// Seed the file with a default (truncating) write.
	writeStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := writeStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  targetPath,
		Chunk: initial,
	}); err != nil {
		t.Fatalf("failed to send seed chunk: %v", err)
	}
	if _, err := writeStream.CloseAndRecv(); err != nil {
		t.Fatalf("seed write CloseAndRecv failed: %v", err)
	}

	// Append two more chunks with append=true on the first message only.
	appendStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := appendStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:   targetPath,
		Chunk:  second,
		Append: true,
	}); err != nil {
		t.Fatalf("failed to send append chunk: %v", err)
	}
	if err := appendStream.Send(&ateenvv1alpha.WriteFileRequest{
		Chunk: third,
	}); err != nil {
		t.Fatalf("failed to send second append chunk: %v", err)
	}
	appendRes, err := appendStream.CloseAndRecv()
	if err != nil {
		t.Fatalf("append CloseAndRecv failed: %v", err)
	}
	if got, want := appendRes.BytesWritten, int64(len(second)+len(third)); got != want {
		t.Fatalf("append bytes written = %d, want %d", got, want)
	}

	// Read back: original content preserved, appended chunks in order.
	got := readFileContent(t, client, ctx, targetPath)
	want := string(initial) + string(second) + string(third)
	if got != want {
		t.Fatalf("content after append = %q, want %q", got, want)
	}
}

func TestAppendCreatesNewFile(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir, ReadBufferSize: 4 * 1024})
	defer cleanup()

	ctx := context.Background()
	targetPath := filepath.Join(tempDir, "brand-new.log")
	content := []byte("first line\n")

	writeStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := writeStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:   targetPath,
		Chunk:  content,
		Mode:   0640,
		Append: true,
	}); err != nil {
		t.Fatalf("failed to send chunk: %v", err)
	}
	res, err := writeStream.CloseAndRecv()
	if err != nil {
		t.Fatalf("append-to-new-file CloseAndRecv failed: %v", err)
	}
	if res.BytesWritten != int64(len(content)) {
		t.Fatalf("bytes written = %d, want %d", res.BytesWritten, len(content))
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("failed to stat written file: %v", err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("expected permissions 0640, got %v", info.Mode().Perm())
	}
	if got := readFileContent(t, client, ctx, targetPath); got != string(content) {
		t.Fatalf("content = %q, want %q", got, string(content))
	}
}

func TestDefaultWriteStillTruncates(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir, ReadBufferSize: 4 * 1024})
	defer cleanup()

	ctx := context.Background()
	targetPath := filepath.Join(tempDir, "replaced.txt")

	writeStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := writeStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  targetPath,
		Chunk: []byte("original content that must go away\n"),
	}); err != nil {
		t.Fatalf("failed to send seed chunk: %v", err)
	}
	if _, err := writeStream.CloseAndRecv(); err != nil {
		t.Fatalf("seed write CloseAndRecv failed: %v", err)
	}

	writeStream, err = client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := writeStream.Send(&ateenvv1alpha.WriteFileRequest{
		Path:  targetPath,
		Chunk: []byte("new"),
	}); err != nil {
		t.Fatalf("failed to send chunk: %v", err)
	}
	if _, err := writeStream.CloseAndRecv(); err != nil {
		t.Fatalf("default write CloseAndRecv failed: %v", err)
	}

	if got, want := readFileContent(t, client, ctx, targetPath), "new"; got != want {
		t.Fatalf("content after default write = %q, want %q", got, want)
	}
}

// readFileContent streams a file back and returns its full content.
func readFileContent(t *testing.T, client ateenvv1alpha.FileSystemServiceClient, ctx context.Context, path string) string {
	t.Helper()
	readStream, err := client.ReadFile(ctx, &ateenvv1alpha.ReadFileRequest{Path: path})
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	var buf bytes.Buffer
	for {
		chunk, err := readStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadFile recv error: %v", err)
		}
		buf.Write(chunk.Data)
	}
	return buf.String()
}
