package filesystem

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// DefaultReadBufferSize is the buffer size for streaming file reads (64 KB).
	DefaultReadBufferSize = 64 * 1024
	// DefaultWorkspace is the default confined root directory for filesystem operations.
	DefaultWorkspace = "/workspace"
)

// Config holds configuration options for the FileSystemService.
type Config struct {
	// RootDirectory confines all file operations to this directory.
	// If set to "/" or empty string with DisableSandbox, boundary checking is disabled.
	RootDirectory string
	// ReadBufferSize is the buffer size used for streaming file reads.
	ReadBufferSize int
}

// DefaultConfig returns the default configuration for FileSystemService.
func DefaultConfig() Config {
	rootDir := DefaultWorkspace
	if env := os.Getenv("WORKSPACE"); env != "" {
		rootDir = env
	} else if env := os.Getenv("WORKDIR"); env != "" {
		rootDir = env
	}
	return Config{
		RootDirectory:  rootDir,
		ReadBufferSize: DefaultReadBufferSize,
	}
}

// Service implements ateenvv1.FileSystemServiceServer.
// It provides in-actor chunked file transfer and manipulation for cmd/ate-env-guest.
type Service struct {
	ateenvv1.UnimplementedFileSystemServiceServer
	rootDir        string
	readBufferSize int
}

// NewService creates a new FileSystemServiceServer instance.
// If configs are provided, the first config is used; otherwise DefaultConfig() is used.
func NewService(configs ...Config) *Service {
	cfg := DefaultConfig()
	if len(configs) > 0 {
		cfg = configs[0]
		if cfg.ReadBufferSize <= 0 {
			cfg.ReadBufferSize = DefaultReadBufferSize
		}
	}

	var cleanedRoot string
	if cfg.RootDirectory != "" && cfg.RootDirectory != "/" {
		cleanedRoot = filepath.Clean(cfg.RootDirectory)
	}

	return &Service{
		rootDir:        cleanedRoot,
		readBufferSize: cfg.ReadBufferSize,
	}
}

// resolveAndValidatePath validates that the target path does not escape the sandbox root directory.
func (s *Service) resolveAndValidatePath(reqPath string) (string, error) {
	if reqPath == "" {
		return "", status.Error(codes.InvalidArgument, "file path cannot be empty")
	}

	var targetPath string
	if filepath.IsAbs(reqPath) {
		targetPath = filepath.Clean(reqPath)
	} else if s.rootDir != "" {
		targetPath = filepath.Clean(filepath.Join(s.rootDir, reqPath))
	} else {
		targetPath = filepath.Clean(reqPath)
	}

	// Boundary check if rootDirectory confinement is enabled
	if s.rootDir != "" && s.rootDir != "/" {
		// Target path must equal rootDir or start with rootDir + separator
		prefix := s.rootDir + string(filepath.Separator)
		if targetPath != s.rootDir && !strings.HasPrefix(targetPath, prefix) {
			return "", status.Errorf(codes.PermissionDenied,
				"access denied: path %q is outside sandbox root directory %q", reqPath, s.rootDir)
		}
	}

	return targetPath, nil
}

// ReadFile streams the contents of a file in chunks to prevent memory bloat/OOM.
func (s *Service) ReadFile(req *ateenvv1.ReadFileRequest, stream ateenvv1.FileSystemService_ReadFileServer) error {
	filePath, err := s.resolveAndValidatePath(req.GetPath())
	if err != nil {
		return err
	}

	f, err := os.Open(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status.Errorf(codes.NotFound, "file %q not found", req.GetPath())
		}
		if errors.Is(err, os.ErrPermission) {
			return status.Errorf(codes.PermissionDenied, "permission denied reading %q", req.GetPath())
		}
		return status.Errorf(codes.Internal, "failed to open file %q: %v", req.GetPath(), err)
	}
	defer f.Close()

	buf := make([]byte, s.readBufferSize)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&ateenvv1.FileChunk{
				Data: buf[:n],
			}); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "failed to read file %q: %v", req.GetPath(), readErr)
		}
	}

	return nil
}

// WriteFile streams file chunks directly to disk with constant O(1) memory.
func (s *Service) WriteFile(stream ateenvv1.FileSystemService_WriteFileServer) error {
	var f *os.File
	var totalBytes int64
	var filePath string
	var reqPath string

	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			// Client finished sending chunks
			if f == nil {
				return status.Error(codes.InvalidArgument, "no file data received")
			}
			if err := f.Close(); err != nil {
				f = nil
				return status.Errorf(codes.Internal, "failed to close file %q: %v", reqPath, err)
			}
			f = nil
			return stream.SendAndClose(&ateenvv1.WriteFileResponse{
				BytesWritten: totalBytes,
			})
		}
		if err != nil {
			return err
		}

		// Initialize file on the first message
		if f == nil {
			reqPath = req.GetPath()
			validatedPath, pathErr := s.resolveAndValidatePath(reqPath)
			if pathErr != nil {
				return pathErr
			}
			filePath = validatedPath

			// Ensure parent directory exists
			dir := filepath.Dir(filePath)
			if dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0755); err != nil {
					return status.Errorf(codes.Internal, "failed to create parent directories for %q: %v", reqPath, err)
				}
			}

			mode := os.FileMode(0644)
			if req.GetMode() != 0 {
				mode = os.FileMode(req.GetMode())
			}

			f, err = os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				if errors.Is(err, os.ErrPermission) {
					return status.Errorf(codes.PermissionDenied, "permission denied opening %q: %v", reqPath, err)
				}
				return status.Errorf(codes.Internal, "failed to create file %q: %v", reqPath, err)
			}
		}

		// Write chunk data
		chunk := req.GetChunk()
		if len(chunk) > 0 {
			n, writeErr := f.Write(chunk)
			if writeErr != nil {
				return status.Errorf(codes.Internal, "failed to write to file %q: %v", reqPath, writeErr)
			}
			totalBytes += int64(n)
		}
	}
}
