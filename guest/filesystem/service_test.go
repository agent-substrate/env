package filesystem

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func setupTestFileSystemServer(t *testing.T, configs ...Config) (ateenvv1.FileSystemServiceClient, func()) {
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
	ateenvv1.RegisterFileSystemServiceServer(server, svc)

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

	client := ateenvv1.NewFileSystemServiceClient(conn)

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

	err = writeStream.Send(&ateenvv1.WriteFileRequest{
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
	readStream, err := client.ReadFile(ctx, &ateenvv1.ReadFileRequest{
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

		req := &ateenvv1.WriteFileRequest{
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
	readStream, err := client.ReadFile(ctx, &ateenvv1.ReadFileRequest{
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
	_ = writeStream.Send(&ateenvv1.WriteFileRequest{
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
	_ = writeStream2.Send(&ateenvv1.WriteFileRequest{
		Path:  traversalPath,
		Chunk: []byte("traversal write"),
	})
	_, err = writeStream2.CloseAndRecv()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for path traversal, got %v", err)
	}

	// 3. Attempt read outside sandbox
	readStream, err := client.ReadFile(ctx, &ateenvv1.ReadFileRequest{
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
	_ = writeStream3.Send(&ateenvv1.WriteFileRequest{
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
	stream, err := client.ReadFile(ctx, &ateenvv1.ReadFileRequest{
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
	stream, err := client.ReadFile(ctx, &ateenvv1.ReadFileRequest{
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
	_ = writeStream.Send(&ateenvv1.WriteFileRequest{
		Path:  "",
		Chunk: []byte("orphan chunk"),
	})

	_, err = writeStream.CloseAndRecv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing path, got %v", err)
	}
}

// dotEntries returns the names of entries in dir that start with ".", i.e.
// leftover WriteFile temp files.
func dotEntries(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %q failed: %v", dir, err)
	}
	var dots []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			dots = append(dots, e.Name())
		}
	}
	return dots
}

func TestStatFile(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir})
	defer cleanup()

	ctx := context.Background()

	// Regular file: name, size, mode, is_dir=false.
	filePath := filepath.Join(tempDir, "stat-target.txt")
	content := []byte("stat me please")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	if err := os.Chmod(filePath, 0640); err != nil {
		t.Fatalf("failed to chmod test file: %v", err)
	}

	fi, err := client.StatFile(ctx, &ateenvv1.StatFileRequest{Path: filePath})
	if err != nil {
		t.Fatalf("StatFile failed: %v", err)
	}
	if fi.Name != "stat-target.txt" {
		t.Fatalf("expected name stat-target.txt, got %q", fi.Name)
	}
	if fi.Size != int64(len(content)) {
		t.Fatalf("expected size %d, got %d", len(content), fi.Size)
	}
	if fi.IsDir {
		t.Fatalf("expected is_dir=false for a regular file")
	}
	if os.FileMode(fi.Mode) != 0640 {
		t.Fatalf("expected mode 0640, got %v", os.FileMode(fi.Mode))
	}
	if fi.ModTime == nil {
		t.Fatalf("expected non-nil mod_time")
	}

	// Directory: is_dir=true.
	dirPath := filepath.Join(tempDir, "stat-dir")
	if err := os.Mkdir(dirPath, 0755); err != nil {
		t.Fatalf("failed to create test dir: %v", err)
	}
	di, err := client.StatFile(ctx, &ateenvv1.StatFileRequest{Path: dirPath})
	if err != nil {
		t.Fatalf("StatFile on directory failed: %v", err)
	}
	if di.Name != "stat-dir" {
		t.Fatalf("expected name stat-dir, got %q", di.Name)
	}
	if !di.IsDir {
		t.Fatalf("expected is_dir=true for a directory")
	}

	// Missing path maps to NotFound.
	_, err = client.StatFile(ctx, &ateenvv1.StatFileRequest{Path: filepath.Join(tempDir, "missing.txt")})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for missing path, got %v", err)
	}
}

func TestListDir(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir})
	defer cleanup()

	ctx := context.Background()

	dirPath := filepath.Join(tempDir, "listing")
	if err := os.MkdirAll(filepath.Join(dirPath, "subdir"), 0755); err != nil {
		t.Fatalf("failed to create test tree: %v", err)
	}
	content := []byte("list me")
	if err := os.WriteFile(filepath.Join(dirPath, "entry.txt"), content, 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	res, err := client.ListDir(ctx, &ateenvv1.ListDirRequest{Path: dirPath})
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(res.Entries), res.Entries)
	}

	var file, sub *ateenvv1.FileInfo
	for _, e := range res.Entries {
		switch e.Name {
		case "entry.txt":
			file = e
		case "subdir":
			sub = e
		}
	}
	if file == nil || sub == nil {
		t.Fatalf("expected entry.txt and subdir in listing, got %v", res.Entries)
	}
	if file.IsDir {
		t.Fatalf("expected entry.txt to not be a directory")
	}
	if file.Size != int64(len(content)) {
		t.Fatalf("expected entry.txt size %d, got %d", len(content), file.Size)
	}
	if !sub.IsDir {
		t.Fatalf("expected subdir to be a directory")
	}

	// Missing directory maps to NotFound.
	_, err = client.ListDir(ctx, &ateenvv1.ListDirRequest{Path: filepath.Join(tempDir, "no-such-dir")})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for missing directory, got %v", err)
	}
}

func TestMakeDir(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir})
	defer cleanup()

	ctx := context.Background()

	// Plain create.
	plainPath := filepath.Join(tempDir, "made")
	if _, err := client.MakeDir(ctx, &ateenvv1.MakeDirRequest{Path: plainPath}); err != nil {
		t.Fatalf("MakeDir failed: %v", err)
	}
	fi, err := os.Stat(plainPath)
	if err != nil || !fi.IsDir() {
		t.Fatalf("expected directory at %s, got info=%v err=%v", plainPath, fi, err)
	}

	// Creating an existing directory maps to AlreadyExists.
	_, err = client.MakeDir(ctx, &ateenvv1.MakeDirRequest{Path: plainPath})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists for existing directory, got %v", err)
	}

	// Nested path without parents fails.
	nestedPath := filepath.Join(tempDir, "a", "b", "c")
	_, err = client.MakeDir(ctx, &ateenvv1.MakeDirRequest{Path: nestedPath})
	if status.Code(err) == codes.OK {
		t.Fatalf("expected error creating nested directory without parents")
	}

	// Nested path with parents succeeds.
	if _, err := client.MakeDir(ctx, &ateenvv1.MakeDirRequest{Path: nestedPath, Parents: true}); err != nil {
		t.Fatalf("MakeDir with parents failed: %v", err)
	}
	fi, err = os.Stat(nestedPath)
	if err != nil || !fi.IsDir() {
		t.Fatalf("expected nested directory at %s, got info=%v err=%v", nestedPath, fi, err)
	}
}

func TestRemovePath(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir})
	defer cleanup()

	ctx := context.Background()

	// Remove a regular file.
	filePath := filepath.Join(tempDir, "remove-me.txt")
	if err := os.WriteFile(filePath, []byte("bye"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	if _, err := client.RemovePath(ctx, &ateenvv1.RemovePathRequest{Path: filePath}); err != nil {
		t.Fatalf("RemovePath failed: %v", err)
	}
	if _, err := os.Lstat(filePath); !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed, stat err=%v", err)
	}

	// Missing path maps to NotFound.
	_, err := client.RemovePath(ctx, &ateenvv1.RemovePathRequest{Path: filepath.Join(tempDir, "already-gone.txt")})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for missing path, got %v", err)
	}

	// Non-empty directory without recursive fails and leaves contents intact.
	dirPath := filepath.Join(tempDir, "full")
	innerPath := filepath.Join(dirPath, "keep.txt")
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		t.Fatalf("failed to create test dir: %v", err)
	}
	if err := os.WriteFile(innerPath, []byte("survivor"), 0644); err != nil {
		t.Fatalf("failed to create inner file: %v", err)
	}
	_, err = client.RemovePath(ctx, &ateenvv1.RemovePathRequest{Path: dirPath})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for non-empty directory, got %v", err)
	}
	if _, err := os.Stat(innerPath); err != nil {
		t.Fatalf("expected directory contents to be intact after failed remove: %v", err)
	}

	// Recursive removal deletes the directory and its contents.
	if _, err := client.RemovePath(ctx, &ateenvv1.RemovePathRequest{Path: dirPath, Recursive: true}); err != nil {
		t.Fatalf("recursive RemovePath failed: %v", err)
	}
	if _, err := os.Lstat(dirPath); !os.IsNotExist(err) {
		t.Fatalf("expected directory to be removed recursively, stat err=%v", err)
	}
}

func TestWriteFileOverwriteAtomicMultiChunk(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir})
	defer cleanup()

	ctx := context.Background()

	// Pre-existing file with different content and mode.
	targetPath := filepath.Join(tempDir, "atomic.txt")
	if err := os.WriteFile(targetPath, []byte("old content that must fully vanish"), 0600); err != nil {
		t.Fatalf("failed to seed existing file: %v", err)
	}

	// Overwrite with chunks spread across three messages.
	parts := [][]byte{[]byte("first-"), []byte("second-"), []byte("third")}
	writeStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	for i, part := range parts {
		req := &ateenvv1.WriteFileRequest{Chunk: part}
		if i == 0 {
			req.Path = targetPath
			req.Mode = 0640
		}
		if err := writeStream.Send(req); err != nil {
			t.Fatalf("failed to send chunk %d: %v", i, err)
		}
	}
	writeRes, err := writeStream.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteFile CloseAndRecv failed: %v", err)
	}

	want := "first-second-third"
	if writeRes.BytesWritten != int64(len(want)) {
		t.Fatalf("expected %d bytes written, got %d", len(want), writeRes.BytesWritten)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(got) != want {
		t.Fatalf("content mismatch: expected %q, got %q", want, got)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("failed to stat written file: %v", err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("expected mode 0640, got %v", info.Mode().Perm())
	}

	// The temp upload file must have been renamed away, not left behind.
	if leftovers := dotEntries(t, tempDir); len(leftovers) != 0 {
		t.Fatalf("expected no leftover temp files after successful write, found %v", leftovers)
	}
}

func TestWriteFileAbortLeavesNoPartial(t *testing.T) {
	tempDir := t.TempDir()
	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: tempDir})
	defer cleanup()

	targetPath := filepath.Join(tempDir, "aborted.txt")
	original := []byte("original content stays")
	if err := os.WriteFile(targetPath, original, 0644); err != nil {
		t.Fatalf("failed to seed existing file: %v", err)
	}

	// Open an upload, send the first message, then abort the stream via
	// context cancellation without completing it.
	ctx, cancel := context.WithCancel(context.Background())
	writeStream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := writeStream.Send(&ateenvv1.WriteFileRequest{
		Path:  targetPath,
		Chunk: []byte("partial data never committed"),
	}); err != nil {
		t.Fatalf("failed to send first chunk: %v", err)
	}
	// Abort with cancellation only. Calling CloseAndRecv here would race:
	// its half-close can reach the server before the cancellation does,
	// and a clean half-close IS a completed upload, which the server
	// rightly commits.
	cancel()

	// Server-side cleanup runs after the cancellation propagates: poll until
	// no temp files (dot-prefixed entries) remain.
	deadline := time.Now().Add(15 * time.Second)
	for {
		leftovers := dotEntries(t, tempDir)
		if len(leftovers) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temp files still present after aborted upload: %v", leftovers)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The target was never renamed over: original content intact.
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("failed to read target file: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("expected original content %q intact after abort, got %q", original, got)
	}
}

func TestMetadataOpsSandboxConfinement(t *testing.T) {
	tempDir := t.TempDir()
	sandboxRoot := filepath.Join(tempDir, "workspace")
	if err := os.MkdirAll(sandboxRoot, 0755); err != nil {
		t.Fatalf("failed to create sandbox root: %v", err)
	}

	client, cleanup := setupTestFileSystemServer(t, Config{RootDirectory: sandboxRoot})
	defer cleanup()

	ctx := context.Background()

	if _, err := client.StatFile(ctx, &ateenvv1.StatFileRequest{Path: "../outside"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("StatFile: expected PermissionDenied for escaping path, got %v", err)
	}
	if _, err := client.ListDir(ctx, &ateenvv1.ListDirRequest{Path: "../outside"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ListDir: expected PermissionDenied for escaping path, got %v", err)
	}
	if _, err := client.MakeDir(ctx, &ateenvv1.MakeDirRequest{Path: "../outside"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("MakeDir: expected PermissionDenied for escaping path, got %v", err)
	}
	if _, err := client.RemovePath(ctx, &ateenvv1.RemovePathRequest{Path: "../outside"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("RemovePath: expected PermissionDenied for escaping path, got %v", err)
	}
}
