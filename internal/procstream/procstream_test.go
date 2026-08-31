package procstream

import (
	"bytes"
	"context"
	"io"
	"testing"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// flakyProcess models a guest whose output streams break every few chunks,
// the way the atenet router's route timeout truncates long-lived streams.
// The process "exits" once exitAfter bytes have been requested.
type flakyProcess struct {
	data       []byte
	perStream  int // bytes served per stream before it breaks
	exitAfter  int // requested offset at which the process counts as exited
	maxOffset  int64
	streamOpen int
}

func (f *flakyProcess) StartProcess(ctx context.Context, in *ateenvv1.StartProcessRequest, opts ...grpc.CallOption) (*ateenvv1.StartProcessResponse, error) {
	return &ateenvv1.StartProcessResponse{ProcessId: "p1"}, nil
}

func (f *flakyProcess) GetProcess(ctx context.Context, in *ateenvv1.GetProcessRequest, opts ...grpc.CallOption) (*ateenvv1.Process, error) {
	st := ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING
	if f.maxOffset >= int64(f.exitAfter) {
		st = ateenvv1.ProcessStatus_PROCESS_STATUS_COMPLETED
	}
	return &ateenvv1.Process{ProcessId: in.GetProcessId(), Status: st}, nil
}

func (f *flakyProcess) KillProcess(ctx context.Context, in *ateenvv1.KillProcessRequest, opts ...grpc.CallOption) (*ateenvv1.KillProcessResponse, error) {
	return &ateenvv1.KillProcessResponse{}, nil
}

func (f *flakyProcess) StreamProcessOutputs(ctx context.Context, in *ateenvv1.StreamProcessOutputsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[ateenvv1.OutputChunk], error) {
	f.streamOpen++
	if in.GetStdoutOffset() > f.maxOffset {
		f.maxOffset = in.GetStdoutOffset()
	}
	return &flakyStream{proc: f, offset: in.GetStdoutOffset(), follow: in.GetFollow()}, nil
}

type flakyStream struct {
	grpc.ClientStream // panics if used; Recv below never touches it
	proc              *flakyProcess
	offset            int64
	follow            bool
	served            int
}

func (s *flakyStream) Recv() (*ateenvv1.OutputChunk, error) {
	if s.offset >= int64(len(s.proc.data)) {
		if !s.follow || s.proc.maxOffset >= int64(s.proc.exitAfter) {
			return nil, io.EOF
		}
		return nil, status.Error(codes.Unavailable, "route timeout")
	}
	if s.served >= s.proc.perStream {
		return nil, status.Error(codes.Unavailable, "route timeout")
	}
	n := min(s.proc.perStream-s.served, 100)
	n = min(n, len(s.proc.data)-int(s.offset))
	chunk := &ateenvv1.OutputChunk{
		Source: ateenvv1.OutputSource_OUTPUT_SOURCE_STDOUT,
		Data:   s.proc.data[s.offset : s.offset+int64(n)],
	}
	s.offset += int64(n)
	s.served += n
	if s.offset > s.proc.maxOffset {
		s.proc.maxOffset = s.offset
	}
	return chunk, nil
}

func (s *flakyStream) Header() (metadata.MD, error) { return nil, nil }
func (s *flakyStream) Trailer() metadata.MD         { return nil }
func (s *flakyStream) CloseSend() error             { return nil }
func (s *flakyStream) Context() context.Context     { return context.Background() }

func TestCollectSurvivesStreamBreaks(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 100) // 1000 bytes
	proc := &flakyProcess{data: data, perStream: 300, exitAfter: 600}

	stdout, stderr, err := Collect(t.Context(), proc, "p1", Options{Follow: true})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !bytes.Equal(stdout, data) {
		t.Fatalf("stdout = %d bytes, want %d; streams opened: %d", len(stdout), len(data), proc.streamOpen)
	}
	if len(stderr) != 0 {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	if proc.streamOpen < 3 {
		t.Fatalf("expected multiple reconnects, got %d streams", proc.streamOpen)
	}
}

func TestCollectGivesUpWithoutProgress(t *testing.T) {
	// A stream that always breaks immediately with the process still running
	// must eventually surface the error instead of looping forever.
	proc := &flakyProcess{data: bytes.Repeat([]byte("x"), 100), perStream: 0, exitAfter: 1000}

	_, _, err := Collect(t.Context(), proc, "p1", Options{Follow: true})
	if err == nil {
		t.Fatal("Collect succeeded despite a permanently broken stream")
	}
}
