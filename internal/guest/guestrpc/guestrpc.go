// Package guestrpc serves the ateenv.v1alpha1 guest data plane inside
// the environment, translating the proto services onto the proc process
// table and the filesystem. It holds no state of its own.
package guestrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/guest/proc"
	pb "github.com/agent-substrate/env/proto/ateenv/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// MaxWait caps Get's long-poll. It stays below the atenet router's
// route timeout (10s by default) so a held poll always completes as a
// response rather than tripping the router's deadline.
const MaxWait = 5 * time.Second

// readChunkBytes is the chunk size of ReadFile's stream.
const readChunkBytes = 64 << 10

// Register registers the guest data plane services on srv, backed by
// table for processes and sys for filesystem access.
func Register(srv *grpc.Server, sys *guestsys.Sys, table *proc.Table) {
	pb.RegisterProcessServer(srv, &processServer{table: table})
	pb.RegisterFileSystemServer(srv, &fileSystemServer{sys: sys})
}

type processServer struct {
	pb.UnimplementedProcessServer
	table *proc.Table
}

func (s *processServer) Exec(ctx context.Context, req *pb.ExecRequest) (*pb.ExecResponse, error) {
	opts := proc.Options{
		Command: req.GetCommand(),
		Dir:     req.GetCwd(),
		Env:     req.GetEnv(),
	}
	res, err := proc.Exec(ctx, opts, req.GetStdin(), time.Duration(req.GetTimeoutMs())*time.Millisecond, 0)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.ExecResponse{
		Stdout:          res.Stdout,
		Stderr:          res.Stderr,
		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
		ExitCode:        int32(res.ExitCode),
		Error:           res.Err,
	}, nil
}

func (s *processServer) Start(ctx context.Context, req *pb.StartRequest) (*pb.StartResponse, error) {
	p, err := s.table.Start(proc.Options{
		Command: req.GetCommand(),
		Dir:     req.GetCwd(),
		Env:     req.GetEnv(),
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.StartResponse{ProcessId: p.ID()}, nil
}

func (s *processServer) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	p, err := s.lookup(req.GetProcessId())
	if err != nil {
		return nil, err
	}
	wait := min(time.Duration(req.GetWaitMs())*time.Millisecond, MaxWait)
	info, stdout, stderr := p.Read(ctx, req.GetStdoutOffset(), req.GetStderrOffset(), wait)
	return &pb.GetResponse{
		Process:      infoProto(info),
		Stdout:       stdout.Data,
		StdoutOffset: stdout.Offset,
		Stderr:       stderr.Data,
		StderrOffset: stderr.Offset,
	}, nil
}

func (s *processServer) WriteStdin(ctx context.Context, req *pb.WriteStdinRequest) (*pb.WriteStdinResponse, error) {
	p, err := s.lookup(req.GetProcessId())
	if err != nil {
		return nil, err
	}
	if err := p.WriteStdin(ctx, req.GetData(), req.GetCloseStdin()); err != nil {
		if errors.Is(err, ctx.Err()) {
			return nil, status.FromContextError(err).Err()
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &pb.WriteStdinResponse{}, nil
}

func (s *processServer) Signal(ctx context.Context, req *pb.SignalRequest) (*pb.SignalResponse, error) {
	p, err := s.lookup(req.GetProcessId())
	if err != nil {
		return nil, err
	}
	if err := p.Signal(syscall.Signal(req.GetSignal())); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &pb.SignalResponse{}, nil
}

func (s *processServer) List(ctx context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	infos := s.table.List()
	resp := &pb.ListResponse{Processes: make([]*pb.ProcessInfo, len(infos))}
	for i, info := range infos {
		resp.Processes[i] = infoProto(info)
	}
	return resp, nil
}

func (s *processServer) lookup(id string) (*proc.Process, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "process_id is required")
	}
	p, ok := s.table.Get(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no process %q", id)
	}
	return p, nil
}

func infoProto(info proc.Info) *pb.ProcessInfo {
	p := &pb.ProcessInfo{
		ProcessId: info.ID,
		Command:   info.Command,
		StartTime: timestamppb.New(info.StartTime),
		StdoutLen: info.StdoutLen,
		StderrLen: info.StderrLen,
	}
	if info.Exited != nil {
		p.Exited = &pb.ExitStatus{
			ExitCode: int32(info.Exited.Code),
			Error:    info.Exited.Err,
			ExitTime: timestamppb.New(info.Exited.Time),
		}
	}
	return p
}

type fileSystemServer struct {
	pb.UnimplementedFileSystemServer
	sys *guestsys.Sys
}

func (s *fileSystemServer) ReadFile(req *pb.ReadFileRequest, stream grpc.ServerStreamingServer[pb.ReadFileResponse]) error {
	path, err := s.resolve(req.GetPath())
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fsError(err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return fsError(err)
	} else if fi.IsDir() {
		return fsError(fmt.Errorf("read %s: %w", path, syscall.EISDIR))
	}

	chunk := make([]byte, readChunkBytes)
	for {
		n, err := f.Read(chunk)
		if n > 0 {
			if err := stream.Send(&pb.ReadFileResponse{Data: chunk[:n]}); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fsError(err)
		}
	}
}

func (s *fileSystemServer) WriteFile(stream grpc.ClientStreamingServer[pb.WriteFileRequest, pb.WriteFileResponse]) error {
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return status.Error(codes.InvalidArgument, "empty WriteFile stream: the first message must carry the path")
	}
	if err != nil {
		return err
	}
	path, err := s.resolve(first.GetPath())
	if err != nil {
		return err
	}
	mode := fs.FileMode(first.GetMode()).Perm()
	if mode == 0 {
		mode = 0o644
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fsError(err)
	}
	// Collect into a temporary file in the target directory and rename
	// it into place, so a failed or abandoned stream never leaves a
	// partial file at path.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fsError(err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	var written int64
	msg := first
	for {
		n, err := tmp.Write(msg.GetData())
		written += int64(n)
		if err != nil {
			return fsError(err)
		}
		msg, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}

	if err := tmp.Chmod(mode); err != nil {
		return fsError(err)
	}
	if err := tmp.Close(); err != nil {
		return fsError(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fsError(err)
	}
	return stream.SendAndClose(&pb.WriteFileResponse{BytesWritten: written})
}

func (s *fileSystemServer) Stat(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
	path, err := s.resolve(req.GetPath())
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fsError(err)
	}
	return &pb.StatResponse{Info: fileInfoProto(fi)}, nil
}

func (s *fileSystemServer) ListDir(ctx context.Context, req *pb.ListDirRequest) (*pb.ListDirResponse, error) {
	path, err := s.resolve(req.GetPath())
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fsError(err)
	}
	resp := &pb.ListDirResponse{Entries: make([]*pb.FileInfo, 0, len(entries))}
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

func (s *fileSystemServer) Mkdir(ctx context.Context, req *pb.MkdirRequest) (*pb.MkdirResponse, error) {
	path, err := s.resolve(req.GetPath())
	if err != nil {
		return nil, err
	}
	if req.GetParents() {
		err = os.MkdirAll(path, 0o755)
	} else {
		err = os.Mkdir(path, 0o755)
	}
	if err != nil {
		return nil, fsError(err)
	}
	return &pb.MkdirResponse{}, nil
}

func (s *fileSystemServer) Remove(ctx context.Context, req *pb.RemoveRequest) (*pb.RemoveResponse, error) {
	path, err := s.resolve(req.GetPath())
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err != nil {
		return nil, fsError(err)
	}
	if req.GetRecursive() {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return nil, fsError(err)
	}
	return &pb.RemoveResponse{}, nil
}

func fileInfoProto(fi fs.FileInfo) *pb.FileInfo {
	return &pb.FileInfo{
		Name:    fi.Name(),
		Size:    fi.Size(),
		Mode:    uint32(fi.Mode().Perm()),
		IsDir:   fi.IsDir(),
		ModTime: timestamppb.New(fi.ModTime()),
	}
}

// resolve cleans path and makes it absolute; relative paths resolve
// against the guest's working directory.
func (s *fileSystemServer) resolve(path string) (string, error) {
	if path == "" {
		return "", status.Error(codes.InvalidArgument, "path is required")
	}
	abs, err := s.sys.Resolve(path)
	if err != nil {
		return "", status.Error(codes.InvalidArgument, err.Error())
	}
	return abs, nil
}

// fsError maps filesystem failures onto gRPC status codes. The errno
// cases run before the fs.Err* ones: Errno.Is folds ENOTEMPTY into
// fs.ErrExist, and "directory not empty" is a precondition failure,
// not an existence conflict.
func fsError(err error) error {
	code := codes.Internal
	switch {
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ENOTEMPTY):
		code = codes.FailedPrecondition
	case errors.Is(err, fs.ErrNotExist):
		code = codes.NotFound
	case errors.Is(err, fs.ErrExist):
		code = codes.AlreadyExists
	case errors.Is(err, fs.ErrPermission):
		code = codes.PermissionDenied
	}
	return status.Error(code, err.Error())
}
