package guestrpc_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/guest/guestrpc"
	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/guest/proc"
	pb "github.com/agent-substrate/env/proto/ateenv/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// newClients starts a real gRPC server backed by a fresh process table
// and the live filesystem, chdirs the test into a temp dir so relative
// paths land there, and returns connected clients.
func newClients(t *testing.T) (pb.ProcessClient, pb.FileSystemClient) {
	t.Helper()
	t.Chdir(t.TempDir())

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	guestrpc.Register(srv, guestsys.New(), proc.NewTable())
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewProcessClient(conn), pb.NewFileSystemClient(conn)
}

// wantCode fails the test unless err carries the given gRPC status code.
func wantCode(t *testing.T, op string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("%s: status = %v (err %v), want %v", op, got, err, want)
	}
}

// drain runs the full long-running-operation loop for a background
// process: it polls Get with chained offsets until the process has
// exited and both streams are fully read, verifying on every response
// that the returned offset equals the requested one (nothing in these
// tests overflows the buffer). It returns the concatenated stdout and
// stderr and the exit status.
func drain(t *testing.T, pc pb.ProcessClient, id string) (stdout, stderr []byte, exited *pb.ExitStatus) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var outOff, errOff int64
	for {
		if time.Now().After(deadline) {
			t.Fatalf("process %s did not exit and drain within deadline", id)
		}
		resp, err := pc.Get(t.Context(), &pb.GetRequest{
			ProcessId:    id,
			StdoutOffset: outOff,
			StderrOffset: errOff,
			WaitMs:       1000,
		})
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got := resp.GetStdoutOffset(); got != outOff {
			t.Fatalf("Get stdout_offset = %d, want requested offset %d", got, outOff)
		}
		if got := resp.GetStderrOffset(); got != errOff {
			t.Fatalf("Get stderr_offset = %d, want requested offset %d", got, errOff)
		}
		stdout = append(stdout, resp.GetStdout()...)
		stderr = append(stderr, resp.GetStderr()...)
		outOff = resp.GetStdoutOffset() + int64(len(resp.GetStdout()))
		errOff = resp.GetStderrOffset() + int64(len(resp.GetStderr()))

		info := resp.GetProcess()
		if info.GetProcessId() != id {
			t.Fatalf("Get process_id = %q, want %q", info.GetProcessId(), id)
		}
		if info.GetExited() != nil && outOff >= info.GetStdoutLen() && errOff >= info.GetStderrLen() {
			return stdout, stderr, info.GetExited()
		}
	}
}

func TestExecRoundtrip(t *testing.T) {
	pc, _ := newClients(t)
	resp, err := pc.Exec(t.Context(), &pb.ExecRequest{
		Command: "echo out; echo err 1>&2; exit 3",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := string(resp.GetStdout()); got != "out\n" {
		t.Errorf("stdout = %q, want %q", got, "out\n")
	}
	if got := string(resp.GetStderr()); got != "err\n" {
		t.Errorf("stderr = %q, want %q", got, "err\n")
	}
	if resp.GetExitCode() != 3 {
		t.Errorf("exit_code = %d, want 3", resp.GetExitCode())
	}
	if resp.GetError() != "" {
		t.Errorf("error = %q, want empty", resp.GetError())
	}
}

func TestExecStdin(t *testing.T) {
	pc, _ := newClients(t)
	resp, err := pc.Exec(t.Context(), &pb.ExecRequest{
		Command: "cat",
		Stdin:   []byte("fed to stdin"),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := string(resp.GetStdout()); got != "fed to stdin" {
		t.Errorf("stdout = %q, want %q", got, "fed to stdin")
	}
	if resp.GetExitCode() != 0 || resp.GetError() != "" {
		t.Errorf("exit_code = %d, error = %q, want 0 and empty", resp.GetExitCode(), resp.GetError())
	}
}

func TestExecTimeoutReportedInBand(t *testing.T) {
	pc, _ := newClients(t)
	resp, err := pc.Exec(t.Context(), &pb.ExecRequest{
		Command:   "sleep 30",
		TimeoutMs: 200,
	})
	if err != nil {
		t.Fatalf("Exec: status = %v, want OK with in-band error", err)
	}
	if !strings.Contains(resp.GetError(), "timed out") {
		t.Errorf("error = %q, want it to report a timeout", resp.GetError())
	}
}

func TestExecEmptyCommand(t *testing.T) {
	pc, _ := newClients(t)
	_, err := pc.Exec(t.Context(), &pb.ExecRequest{})
	wantCode(t, "Exec with empty command", err, codes.InvalidArgument)
}

func TestStartGetLoop(t *testing.T) {
	pc, _ := newClients(t)
	start, err := pc.Start(t.Context(), &pb.StartRequest{
		Command: "echo out1; echo err1 1>&2; sleep 0.2; echo out2; sleep 0.2; echo out3; exit 5",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if start.GetProcessId() == "" {
		t.Fatal("Start returned an empty process_id")
	}

	stdout, stderr, exited := drain(t, pc, start.GetProcessId())
	if got, want := string(stdout), "out1\nout2\nout3\n"; got != want {
		t.Errorf("concatenated stdout = %q, want %q", got, want)
	}
	if got, want := string(stderr), "err1\n"; got != want {
		t.Errorf("concatenated stderr = %q, want %q", got, want)
	}
	if exited.GetExitCode() != 5 {
		t.Errorf("exited.exit_code = %d, want 5", exited.GetExitCode())
	}
	if exited.GetError() != "" {
		t.Errorf("exited.error = %q, want empty", exited.GetError())
	}
}

func TestGetUnknownProcess(t *testing.T) {
	pc, _ := newClients(t)
	_, err := pc.Get(t.Context(), &pb.GetRequest{ProcessId: "no-such-process"})
	wantCode(t, "Get with unknown process_id", err, codes.NotFound)
}

func TestGetEmptyProcessID(t *testing.T) {
	pc, _ := newClients(t)
	_, err := pc.Get(t.Context(), &pb.GetRequest{})
	wantCode(t, "Get with empty process_id", err, codes.InvalidArgument)
}

func TestWriteStdin(t *testing.T) {
	pc, _ := newClients(t)
	start, err := pc.Start(t.Context(), &pb.StartRequest{Command: "cat"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := start.GetProcessId()

	if _, err := pc.WriteStdin(t.Context(), &pb.WriteStdinRequest{
		ProcessId: id,
		Data:      []byte("hello "),
	}); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}
	if _, err := pc.WriteStdin(t.Context(), &pb.WriteStdinRequest{
		ProcessId:  id,
		Data:       []byte("world\n"),
		CloseStdin: true,
	}); err != nil {
		t.Fatalf("WriteStdin with close_stdin: %v", err)
	}

	stdout, _, exited := drain(t, pc, id)
	if got, want := string(stdout), "hello world\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if exited.GetExitCode() != 0 || exited.GetError() != "" {
		t.Errorf("exited = code %d error %q, want 0 and empty", exited.GetExitCode(), exited.GetError())
	}

	_, err = pc.WriteStdin(t.Context(), &pb.WriteStdinRequest{ProcessId: id, Data: []byte("late")})
	wantCode(t, "WriteStdin to exited process", err, codes.FailedPrecondition)
}

func TestSignalAndList(t *testing.T) {
	pc, _ := newClients(t)
	start, err := pc.Start(t.Context(), &pb.StartRequest{Command: "sleep 60"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := start.GetProcessId()

	list, err := pc.List(t.Context(), &pb.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, p := range list.GetProcesses() {
		if p.GetProcessId() == id {
			found = true
			if p.GetCommand() != "sleep 60" {
				t.Errorf("listed command = %q, want %q", p.GetCommand(), "sleep 60")
			}
		}
	}
	if !found {
		t.Fatalf("List did not include process %s", id)
	}

	if _, err := pc.Signal(t.Context(), &pb.SignalRequest{ProcessId: id, Signal: 9}); err != nil {
		t.Fatalf("Signal SIGKILL: %v", err)
	}

	_, _, exited := drain(t, pc, id)
	if !strings.Contains(exited.GetError(), "kill") {
		t.Errorf("exited.error = %q, want it to report the kill signal", exited.GetError())
	}

	_, err = pc.Signal(t.Context(), &pb.SignalRequest{ProcessId: id, Signal: 15})
	wantCode(t, "Signal on exited process", err, codes.FailedPrecondition)
}

func TestWriteFile(t *testing.T) {
	_, fc := newClients(t)
	parts := [][]byte{[]byte("part-one|"), []byte("part-two|"), []byte("part-three")}

	stream, err := fc.WriteFile(t.Context())
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := stream.Send(&pb.WriteFileRequest{
		Path: "nested/deep/file.txt",
		Mode: 0o640,
		Data: parts[0],
	}); err != nil {
		t.Fatalf("Send first message: %v", err)
	}
	for _, part := range parts[1:] {
		if err := stream.Send(&pb.WriteFileRequest{Data: part}); err != nil {
			t.Fatalf("Send data message: %v", err)
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}

	want := bytes.Join(parts, nil)
	if resp.GetBytesWritten() != int64(len(want)) {
		t.Errorf("bytes_written = %d, want %d", resp.GetBytesWritten(), len(want))
	}
	got, err := os.ReadFile("nested/deep/file.txt")
	if err != nil {
		t.Fatalf("reading written file (parent dirs should be auto-created): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("file content = %q, want %q", got, want)
	}
	fi, err := os.Stat("nested/deep/file.txt")
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("file mode = %v, want %v", fi.Mode().Perm(), os.FileMode(0o640))
	}
}

func TestReadFile(t *testing.T) {
	_, fc := newClients(t)

	// Larger than the server's 64 KiB chunk so several chunks stream back.
	want := make([]byte, 200_000)
	for i := range want {
		want[i] = byte(i % 251)
	}
	if err := os.WriteFile("big.bin", want, 0o600); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}

	stream, err := fc.ReadFile(t.Context(), &pb.ReadFileRequest{Path: "big.bin"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got []byte
	chunks := 0
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, resp.GetData()...)
		chunks++
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("streamed %d bytes, want %d bytes matching what was written", len(got), len(want))
	}
	if chunks < 2 {
		t.Errorf("received %d chunks for a %d byte file, want several", chunks, len(want))
	}
}

func TestReadFileMissing(t *testing.T) {
	_, fc := newClients(t)
	stream, err := fc.ReadFile(t.Context(), &pb.ReadFileRequest{Path: "does-not-exist"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	_, err = stream.Recv()
	wantCode(t, "ReadFile of missing file", err, codes.NotFound)
}

func TestReadFileDirectory(t *testing.T) {
	_, fc := newClients(t)
	if err := os.Mkdir("adir", 0o755); err != nil {
		t.Fatal(err)
	}
	stream, err := fc.ReadFile(t.Context(), &pb.ReadFileRequest{Path: "adir"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	_, err = stream.Recv()
	wantCode(t, "ReadFile of a directory", err, codes.FailedPrecondition)
}

func TestStat(t *testing.T) {
	_, fc := newClients(t)
	content := []byte("stat me please")
	if err := os.WriteFile("f.txt", content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod("f.txt", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("d", 0o755); err != nil {
		t.Fatal(err)
	}

	resp, err := fc.Stat(t.Context(), &pb.StatRequest{Path: "f.txt"})
	if err != nil {
		t.Fatalf("Stat file: %v", err)
	}
	info := resp.GetInfo()
	if info.GetName() != "f.txt" {
		t.Errorf("name = %q, want %q", info.GetName(), "f.txt")
	}
	if info.GetSize() != int64(len(content)) {
		t.Errorf("size = %d, want %d", info.GetSize(), len(content))
	}
	if info.GetIsDir() {
		t.Error("is_dir = true for a regular file")
	}
	if info.GetMode() != 0o600 {
		t.Errorf("mode = %o, want %o", info.GetMode(), 0o600)
	}

	resp, err = fc.Stat(t.Context(), &pb.StatRequest{Path: "d"})
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if !resp.GetInfo().GetIsDir() {
		t.Error("is_dir = false for a directory")
	}

	_, err = fc.Stat(t.Context(), &pb.StatRequest{Path: "missing"})
	wantCode(t, "Stat of missing path", err, codes.NotFound)
}

func TestListDir(t *testing.T) {
	_, fc := newClients(t)
	if err := os.Mkdir("dir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("dir/a.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("dir/sub", 0o755); err != nil {
		t.Fatal(err)
	}

	resp, err := fc.ListDir(t.Context(), &pb.ListDirRequest{Path: "dir"})
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	entries := resp.GetEntries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	byName := map[string]*pb.FileInfo{}
	for _, e := range entries {
		byName[e.GetName()] = e
	}
	file, ok := byName["a.txt"]
	if !ok {
		t.Fatal("entry a.txt missing from listing")
	}
	if file.GetIsDir() || file.GetSize() != int64(len("hello")) {
		t.Errorf("a.txt: is_dir=%v size=%d, want file of size 5", file.GetIsDir(), file.GetSize())
	}
	sub, ok := byName["sub"]
	if !ok {
		t.Fatal("entry sub missing from listing")
	}
	if !sub.GetIsDir() {
		t.Error("sub: is_dir = false, want true")
	}
}

func TestMkdir(t *testing.T) {
	_, fc := newClients(t)

	if _, err := fc.Mkdir(t.Context(), &pb.MkdirRequest{Path: "d1"}); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	fi, err := os.Stat("d1")
	if err != nil || !fi.IsDir() {
		t.Fatalf("d1 after Mkdir: fi=%v err=%v, want a directory", fi, err)
	}

	_, err = fc.Mkdir(t.Context(), &pb.MkdirRequest{Path: "d1"})
	wantCode(t, "Mkdir of existing dir", err, codes.AlreadyExists)

	_, err = fc.Mkdir(t.Context(), &pb.MkdirRequest{Path: "x/y/z"})
	if status.Code(err) == codes.OK {
		t.Fatal("Mkdir of nested path without parents succeeded, want error")
	}

	if _, err := fc.Mkdir(t.Context(), &pb.MkdirRequest{Path: "x/y/z", Parents: true}); err != nil {
		t.Fatalf("Mkdir with parents: %v", err)
	}
	fi, err = os.Stat("x/y/z")
	if err != nil || !fi.IsDir() {
		t.Fatalf("x/y/z after Mkdir with parents: fi=%v err=%v, want a directory", fi, err)
	}
}

func TestRemove(t *testing.T) {
	_, fc := newClients(t)

	if err := os.WriteFile("gone.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fc.Remove(t.Context(), &pb.RemoveRequest{Path: "gone.txt"}); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if _, err := os.Lstat("gone.txt"); !os.IsNotExist(err) {
		t.Fatalf("gone.txt still present after Remove: %v", err)
	}

	_, err := fc.Remove(t.Context(), &pb.RemoveRequest{Path: "gone.txt"})
	wantCode(t, "Remove of missing path", err, codes.NotFound)

	if err := os.MkdirAll("full/inner", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("full/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = fc.Remove(t.Context(), &pb.RemoveRequest{Path: "full"})
	wantCode(t, "Remove of non-empty dir", err, codes.FailedPrecondition)
	if _, err := os.Lstat("full/f.txt"); err != nil {
		t.Fatalf("full/f.txt disappeared after failed non-recursive Remove: %v", err)
	}

	if _, err := fc.Remove(t.Context(), &pb.RemoveRequest{Path: "full", Recursive: true}); err != nil {
		t.Fatalf("Remove recursive: %v", err)
	}
	if _, err := os.Lstat("full"); !os.IsNotExist(err) {
		t.Fatalf("full still present after recursive Remove: %v", err)
	}
}
