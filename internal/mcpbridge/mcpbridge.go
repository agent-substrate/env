// Package mcpbridge exposes one environment's data plane as MCP tools,
// so an unmodified agent harness can drive it over stdio. Each tool
// call becomes an ateenv.v1alpha1 RPC against ate-env-api, addressed
// with ate-env-id request metadata; the environment's lifecycle —
// create, suspend, resume, reap — never shows through.
//
// The long-running-operation loop lives here: the bridge remembers the
// read offsets of every process it started, so check_process returns
// exactly the output produced since the last check.
package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	pb "github.com/agent-substrate/env/proto/ateenv/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// metadataKey mirrors grpcproxy.MetadataKey; the bridge is a client of
// the wire contract, not of the server's internals.
const metadataKey = "ate-env-id"

// writeChunkBytes is the chunk size of write_file's upload stream.
const writeChunkBytes = 64 << 10

// maxReadFileBytes caps read_file, which returns the whole file as one
// tool result.
const maxReadFileBytes = 10 << 20

// defaultShellTimeout bounds shell commands that do not ask for a
// timeout, so a hung command cannot wedge the harness forever.
const defaultShellTimeout = 2 * time.Minute

// defaultCheckWait is check_process's long-poll when the call does not
// set one; the guest caps it below the router's route timeout either
// way.
const defaultCheckWait = 3 * time.Second

// maxShellOutputBytes caps what shell accumulates per stream across its
// polling loop; beyond it the oldest bytes are dropped, matching the
// guest's own buffers.
const maxShellOutputBytes = 1 << 20

// bridge holds the per-process read offsets behind the tools.
type bridge struct {
	envID string
	proc  pb.ProcessClient
	fs    pb.FileSystemClient

	mu      sync.Mutex
	offsets map[string]*offsets
}

// offsets tracks how far the bridge has read a process's output. Its
// mutex serializes check_process calls for the same process, so
// concurrent checks cannot double-read or move the offsets backwards.
type offsets struct {
	sync.Mutex
	stdout, stderr int64
}

// NewServer returns an MCP server exposing environment envID, reached
// through conn — an ate-env-api connection.
func NewServer(conn grpc.ClientConnInterface, envID string) *mcp.Server {
	b := &bridge{
		envID:   envID,
		proc:    pb.NewProcessClient(conn),
		fs:      pb.NewFileSystemClient(conn),
		offsets: make(map[string]*offsets),
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "ate-env", Version: "1.0.0"}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "shell",
		Description: "Run a shell command in the environment and return its output.",
	}, b.shell)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a file from the environment.",
	}, b.readFile)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write_file",
		Description: "Write a file in the environment, creating parent directories as needed.",
	}, b.writeFile)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_dir",
		Description: "List a directory in the environment.",
	}, b.listDir)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "start_process",
		Description: "Start a background process in the environment and return its process id.",
	}, b.startProcess)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "check_process",
		Description: "Check a background process: returns output produced since the last check and whether it is still running.",
	}, b.checkProcess)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write_stdin",
		Description: "Write to a background process's standard input.",
	}, b.writeStdin)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "stop_process",
		Description: "Signal a background process (SIGTERM by default).",
	}, b.stopProcess)

	return srv
}

// ctx addresses an outgoing RPC to the bridge's environment.
func (b *bridge) ctx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, metadataKey, b.envID)
}

func text(s string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}, nil, nil
}

type shellArgs struct {
	Command   string `json:"command" jsonschema:"the shell command line to run"`
	Cwd       string `json:"cwd,omitempty" jsonschema:"working directory (defaults to the environment's)"`
	TimeoutMs int64  `json:"timeout_ms,omitempty" jsonschema:"timeout in milliseconds (default 120000)"`
}

// shell runs a command as a background process and polls it by offset
// to completion. A single Exec would be simpler, but every call here
// crosses the atenet router, whose route timeout is far shorter than a
// build or a test run — the polling loop is what lets a command run
// for minutes over calls that each finish in seconds.
func (b *bridge) shell(ctx context.Context, req *mcp.CallToolRequest, args shellArgs) (*mcp.CallToolResult, any, error) {
	timeout := time.Duration(args.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultShellTimeout
	}
	start, err := b.proc.Start(b.ctx(ctx), &pb.StartRequest{
		Command: args.Command,
		Cwd:     args.Cwd,
	})
	if err != nil {
		return nil, nil, err
	}
	id := start.GetProcessId()

	var stdout, stderr tailBuffer
	var stdoutOff, stderrOff int64
	deadline := time.Now().Add(timeout)
	for {
		res, err := b.proc.Get(b.ctx(ctx), &pb.GetRequest{
			ProcessId:    id,
			StdoutOffset: stdoutOff,
			StderrOffset: stderrOff,
			WaitMs:       defaultCheckWait.Milliseconds(),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("command still runs as process %s, but polling it failed: %w", id, err)
		}
		stdout.markGap(res.GetStdoutOffset() > stdoutOff)
		stderr.markGap(res.GetStderrOffset() > stderrOff)
		stdout.write(res.GetStdout())
		stderr.write(res.GetStderr())
		stdoutOff = res.GetStdoutOffset() + int64(len(res.GetStdout()))
		stderrOff = res.GetStderrOffset() + int64(len(res.GetStderr()))

		p := res.GetProcess()
		if exited := p.GetExited(); exited != nil && stdoutOff >= p.GetStdoutLen() && stderrOff >= p.GetStderrLen() {
			out := formatOutput(stdout.data, stderr.data)
			if stdout.truncated || stderr.truncated {
				out += "\n[output truncated: oldest bytes dropped]"
			}
			if exited.GetError() != "" {
				return nil, nil, fmt.Errorf("command did not complete: %s\n%s", exited.GetError(), out)
			}
			if exited.GetExitCode() != 0 {
				out += fmt.Sprintf("\n[exit code %d]", exited.GetExitCode())
			}
			return text(out)
		}
		if time.Now().After(deadline) {
			b.proc.Signal(b.ctx(ctx), &pb.SignalRequest{ProcessId: id, Signal: 9})
			return nil, nil, fmt.Errorf("command timed out after %s and was killed\n%s", timeout, formatOutput(stdout.data, stderr.data))
		}
	}
}

// tailBuffer keeps the newest maxShellOutputBytes of a stream.
type tailBuffer struct {
	data      []byte
	truncated bool
}

func (b *tailBuffer) write(p []byte) {
	b.data = append(b.data, p...)
	if over := len(b.data) - maxShellOutputBytes; over > 0 {
		b.data = b.data[over:]
		b.truncated = true
	}
}

func (b *tailBuffer) markGap(gap bool) {
	if gap {
		b.truncated = true
	}
}

// formatOutput renders captured stdout and stderr as one text block.
func formatOutput(stdout, stderr []byte) string {
	out := string(stdout)
	if len(stderr) > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "[stderr]\n" + string(stderr)
	}
	return out
}

type readFileArgs struct {
	Path string `json:"path" jsonschema:"path of the file to read"`
}

func (b *bridge) readFile(ctx context.Context, req *mcp.CallToolRequest, args readFileArgs) (*mcp.CallToolResult, any, error) {
	stream, err := b.fs.ReadFile(b.ctx(ctx), &pb.ReadFileRequest{Path: args.Path})
	if err != nil {
		return nil, nil, err
	}
	var content []byte
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return text(string(content))
			}
			return nil, nil, err
		}
		content = append(content, resp.GetData()...)
		if len(content) > maxReadFileBytes {
			return nil, nil, fmt.Errorf("%s is larger than %d bytes; read it in pieces with shell instead", args.Path, maxReadFileBytes)
		}
	}
}

type writeFileArgs struct {
	Path    string `json:"path" jsonschema:"path of the file to write"`
	Content string `json:"content" jsonschema:"the full file content"`
	Mode    uint32 `json:"mode,omitempty" jsonschema:"POSIX file mode (default 0644)"`
}

func (b *bridge) writeFile(ctx context.Context, req *mcp.CallToolRequest, args writeFileArgs) (*mcp.CallToolResult, any, error) {
	stream, err := b.fs.WriteFile(b.ctx(ctx))
	if err != nil {
		return nil, nil, err
	}
	content := []byte(args.Content)
	msg := &pb.WriteFileRequest{Path: args.Path, Mode: args.Mode}
	for {
		n := min(len(content), writeChunkBytes)
		msg.Data, content = content[:n], content[n:]
		if err := stream.Send(msg); err != nil {
			break // the send error surfaces via CloseAndRecv
		}
		if len(content) == 0 {
			break
		}
		msg = &pb.WriteFileRequest{}
	}
	res, err := stream.CloseAndRecv()
	if err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("wrote %d bytes to %s", res.GetBytesWritten(), args.Path))
}

type listDirArgs struct {
	Path string `json:"path" jsonschema:"path of the directory to list"`
}

func (b *bridge) listDir(ctx context.Context, req *mcp.CallToolRequest, args listDirArgs) (*mcp.CallToolResult, any, error) {
	res, err := b.fs.ListDir(b.ctx(ctx), &pb.ListDirRequest{Path: args.Path})
	if err != nil {
		return nil, nil, err
	}
	if len(res.GetEntries()) == 0 {
		return text(args.Path + " is empty")
	}
	var out strings.Builder
	for _, e := range res.GetEntries() {
		if e.GetIsDir() {
			fmt.Fprintf(&out, "%s/\n", e.GetName())
		} else {
			fmt.Fprintf(&out, "%s\t%d\n", e.GetName(), e.GetSize())
		}
	}
	return text(strings.TrimRight(out.String(), "\n"))
}

type startProcessArgs struct {
	Command string `json:"command" jsonschema:"the shell command line to run in the background"`
	Cwd     string `json:"cwd,omitempty" jsonschema:"working directory (defaults to the environment's)"`
}

func (b *bridge) startProcess(ctx context.Context, req *mcp.CallToolRequest, args startProcessArgs) (*mcp.CallToolResult, any, error) {
	res, err := b.proc.Start(b.ctx(ctx), &pb.StartRequest{
		Command: args.Command,
		Cwd:     args.Cwd,
	})
	if err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	b.offsets[res.GetProcessId()] = &offsets{}
	b.mu.Unlock()
	return text("started process " + res.GetProcessId())
}

type checkProcessArgs struct {
	ProcessID string `json:"process_id" jsonschema:"id returned by start_process"`
	WaitMs    int64  `json:"wait_ms,omitempty" jsonschema:"wait up to this long for new output (default 3000)"`
}

func (b *bridge) checkProcess(ctx context.Context, req *mcp.CallToolRequest, args checkProcessArgs) (*mcp.CallToolResult, any, error) {
	b.mu.Lock()
	off := b.offsets[args.ProcessID]
	if off == nil {
		off = &offsets{}
		b.offsets[args.ProcessID] = off
	}
	b.mu.Unlock()
	off.Lock()
	defer off.Unlock()

	wait := args.WaitMs
	if wait <= 0 {
		wait = defaultCheckWait.Milliseconds()
	}
	res, err := b.proc.Get(b.ctx(ctx), &pb.GetRequest{
		ProcessId:    args.ProcessID,
		StdoutOffset: off.stdout,
		StderrOffset: off.stderr,
		WaitMs:       wait,
	})
	if err != nil {
		return nil, nil, err
	}

	gap := res.GetStdoutOffset() > off.stdout || res.GetStderrOffset() > off.stderr
	off.stdout = res.GetStdoutOffset() + int64(len(res.GetStdout()))
	off.stderr = res.GetStderrOffset() + int64(len(res.GetStderr()))

	status := "still running"
	if exited := res.GetProcess().GetExited(); exited != nil {
		if exited.GetError() != "" {
			status = "exited: " + exited.GetError()
		} else {
			status = fmt.Sprintf("exited with code %d", exited.GetExitCode())
		}
	}
	out := formatOutput(res.GetStdout(), res.GetStderr())
	if out == "" {
		out = "[no new output]"
	}
	if gap {
		out += "\n[some output was dropped: the process outran the buffer]"
	}
	return text(out + "\n[" + status + "]")
}

type writeStdinArgs struct {
	ProcessID  string `json:"process_id" jsonschema:"id returned by start_process"`
	Data       string `json:"data" jsonschema:"bytes to write to the process's stdin"`
	CloseStdin bool   `json:"close_stdin,omitempty" jsonschema:"close stdin after writing"`
}

func (b *bridge) writeStdin(ctx context.Context, req *mcp.CallToolRequest, args writeStdinArgs) (*mcp.CallToolResult, any, error) {
	_, err := b.proc.WriteStdin(b.ctx(ctx), &pb.WriteStdinRequest{
		ProcessId:  args.ProcessID,
		Data:       []byte(args.Data),
		CloseStdin: args.CloseStdin,
	})
	if err != nil {
		return nil, nil, err
	}
	return text("ok")
}

type stopProcessArgs struct {
	ProcessID string `json:"process_id" jsonschema:"id returned by start_process"`
	Signal    int32  `json:"signal,omitempty" jsonschema:"POSIX signal number (default 15, SIGTERM)"`
}

func (b *bridge) stopProcess(ctx context.Context, req *mcp.CallToolRequest, args stopProcessArgs) (*mcp.CallToolResult, any, error) {
	sig := args.Signal
	if sig == 0 {
		sig = 15 // SIGTERM
	}
	_, err := b.proc.Signal(b.ctx(ctx), &pb.SignalRequest{
		ProcessId: args.ProcessID,
		Signal:    sig,
	})
	if err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("sent signal %d to %s", sig, args.ProcessID))
}
