package filesystem

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
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
// Chunks land in a temporary file next to the target, which is renamed into
// place only when the upload completes — a failed or abandoned stream never
// leaves a partial file at the requested path.
func (s *Service) WriteFile(stream ateenvv1.FileSystemService_WriteFileServer) error {
	var f *os.File
	var totalBytes int64
	var filePath string
	var reqPath string
	var mode os.FileMode

	defer func() {
		if f != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			// Client finished sending chunks
			if f == nil {
				return status.Error(codes.InvalidArgument, "no file data received")
			}
			if err := f.Chmod(mode); err != nil {
				return status.Errorf(codes.Internal, "failed to set mode on %q: %v", reqPath, err)
			}
			if err := f.Close(); err != nil {
				return status.Errorf(codes.Internal, "failed to close file %q: %v", reqPath, err)
			}
			if err := os.Rename(f.Name(), filePath); err != nil {
				return status.Errorf(codes.Internal, "failed to finalize file %q: %v", reqPath, err)
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

			mode = os.FileMode(0644)
			if req.GetMode() != 0 {
				mode = os.FileMode(req.GetMode())
			}

			f, err = os.CreateTemp(dir, "."+filepath.Base(filePath)+".*")
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

// StatFile returns metadata for a file or directory.
func (s *Service) StatFile(ctx context.Context, req *ateenvv1.StatFileRequest) (*ateenvv1.FileInfo, error) {
	path, err := s.resolveAndValidatePath(req.GetPath())
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fsError(req.GetPath(), err)
	}
	return fileInfoProto(fi), nil
}

// ListDir returns the immediate entries of a directory.
func (s *Service) ListDir(ctx context.Context, req *ateenvv1.ListDirRequest) (*ateenvv1.ListDirResponse, error) {
	path, err := s.resolveAndValidatePath(req.GetPath())
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fsError(req.GetPath(), err)
	}
	resp := &ateenvv1.ListDirResponse{Entries: make([]*ateenvv1.FileInfo, 0, len(entries))}
	for _, entry := range entries {
		fi, err := entry.Info()
		if err != nil {
			// The entry disappeared between the listing and the stat.
			continue
		}
		resp.Entries = append(resp.Entries, fileInfoProto(fi))
	}
	return resp, nil
}

// MakeDir creates a directory.
func (s *Service) MakeDir(ctx context.Context, req *ateenvv1.MakeDirRequest) (*ateenvv1.MakeDirResponse, error) {
	path, err := s.resolveAndValidatePath(req.GetPath())
	if err != nil {
		return nil, err
	}
	if req.GetParents() {
		err = os.MkdirAll(path, 0o755)
	} else {
		err = os.Mkdir(path, 0o755)
	}
	if err != nil {
		return nil, fsError(req.GetPath(), err)
	}
	return &ateenvv1.MakeDirResponse{}, nil
}

// RemovePath deletes a file or directory.
func (s *Service) RemovePath(ctx context.Context, req *ateenvv1.RemovePathRequest) (*ateenvv1.RemovePathResponse, error) {
	path, err := s.resolveAndValidatePath(req.GetPath())
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err != nil {
		return nil, fsError(req.GetPath(), err)
	}
	if req.GetRecursive() {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return nil, fsError(req.GetPath(), err)
	}
	return &ateenvv1.RemovePathResponse{}, nil
}

func fileInfoProto(fi os.FileInfo) *ateenvv1.FileInfo {
	return &ateenvv1.FileInfo{
		Name:    fi.Name(),
		Size:    fi.Size(),
		Mode:    uint32(fi.Mode().Perm()),
		IsDir:   fi.IsDir(),
		ModTime: timestamppb.New(fi.ModTime()),
	}
}

// fsError maps filesystem failures onto gRPC status codes. The errno cases
// run before the fs.Err* ones: Errno.Is folds ENOTEMPTY into fs.ErrExist,
// and "directory not empty" is a precondition failure, not an existence
// conflict.
func fsError(path string, err error) error {
	code := codes.Internal
	switch {
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ENOTEMPTY):
		code = codes.FailedPrecondition
	case errors.Is(err, os.ErrNotExist):
		code = codes.NotFound
	case errors.Is(err, os.ErrExist):
		code = codes.AlreadyExists
	case errors.Is(err, os.ErrPermission):
		code = codes.PermissionDenied
	}
	return status.Errorf(code, "%s: %v", path, err)
}
