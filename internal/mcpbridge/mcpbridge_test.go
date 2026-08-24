package mcpbridge_test

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/grpcmux"
	"github.com/agent-substrate/env/internal/grpcproxy"
	"github.com/agent-substrate/env/internal/guest/guestrpc"
	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/guest/proc"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/mcpbridge"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// pollDeadline bounds every output-polling loop. It is deliberately
// generous: the loops exit as soon as the condition holds, so a large
// bound costs nothing on the happy path and only guards against hangs.
const pollDeadline = 30 * time.Second

// newSession builds the full data plane the bridge fronts — a guest
// env "env-1" behind a fake router, an ate.Client on a fake control
// plane, and the api-side proxy server — then connects an in-memory
// MCP client/server pair over it and returns the client session.
func newSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	t.Chdir(t.TempDir())

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.State(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
	}
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	client, err := ate.New(ate.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	// The api-side server: the opaque proxy forwarding every method to
	// the guest picked out by request metadata.
	grpcServer := grpc.NewServer(
		grpc.ForceServerCodec(grpcproxy.Codec()),
		grpc.UnknownServiceHandler(grpcproxy.StreamHandler(
			func(ctx context.Context, envID string) (*grpc.ClientConn, error) {
				return client.GuestConn(envID)
			},
		)),
	)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpcmux.Server(grpcmux.Handler(grpcServer, http.NotFoundHandler()))
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Close() })

	// The guest env the bridge addresses.
	guestGrpc := grpc.NewServer()
	guestrpc.Register(guestGrpc, guestsys.New(), proc.NewTable())
	router.Register("env-1", grpcmux.Handler(guestGrpc, http.NotFoundHandler()))
	t.Cleanup(guestGrpc.Stop)
	if err := client.Create(t.Context(), ate.CreateOptions{
		ID:        "env-1",
		Template:  "default-env",
		Namespace: "envs",
	}); err != nil {
		t.Fatalf("creating env-1: %v", err)
	}

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := mcpbridge.NewServer(conn, "env-1").Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting bridge server: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "mcpbridge-test", Version: "1.0.0"}, nil)
	session, err := mcpClient.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting MCP client: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// callTool calls a tool and fails only on protocol-level errors, so
// callers can assert on IsError results.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%q, %v): %v", name, args, err)
	}
	return res
}

// callText calls a tool, fails the test on an IsError result, and
// returns the concatenated text content.
func callText(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res := callTool(t, session, name, args)
	text := resultText(res)
	if res.IsError {
		t.Fatalf("tool %q(%v) returned an error result: %s", name, args, text)
	}
	return text
}

func resultText(res *mcp.CallToolResult) string {
	var out strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	return out.String()
}

func TestShell(t *testing.T) {
	session := newSession(t)

	if got := callText(t, session, "shell", map[string]any{"command": "printf hello"}); got != "hello" {
		t.Errorf("shell printf hello = %q, want %q", got, "hello")
	}

	// A nonzero exit is an outcome, not an error result: the exit code
	// is reported in-band so the model can see it.
	if got := callText(t, session, "shell", map[string]any{"command": "printf partial; exit 3"}); !strings.Contains(got, "[exit code 3]") {
		t.Errorf("shell exit 3 = %q, want it to contain %q", got, "[exit code 3]")
	}

	if got := callText(t, session, "shell", map[string]any{"command": "echo oops >&2"}); !strings.Contains(got, "[stderr]") {
		t.Errorf("shell with stderr = %q, want it to contain %q", got, "[stderr]")
	}
}

func TestFileRoundtrip(t *testing.T) {
	session := newSession(t)

	// 200 KiB, so both the write_file upload and the read_file download
	// must cross their 64 KiB chunk boundaries.
	content := strings.Repeat("0123456789abcdef", 200<<10/16)

	wrote := callText(t, session, "write_file", map[string]any{"path": "big.txt", "content": content})
	if !strings.Contains(wrote, "wrote 204800 bytes") {
		t.Errorf("write_file = %q, want it to report writing %d bytes", wrote, len(content))
	}

	if got := callText(t, session, "read_file", map[string]any{"path": "big.txt"}); got != content {
		t.Errorf("read_file roundtrip: got %d bytes, want %d (equal = %v)", len(got), len(content), got == content)
	}

	// A missing file is a tool error result, not a protocol error.
	res := callTool(t, session, "read_file", map[string]any{"path": "no-such-file.txt"})
	if !res.IsError {
		t.Errorf("read_file of a missing path: IsError = false, want true; text = %q", resultText(res))
	}
}

func TestListDir(t *testing.T) {
	session := newSession(t)

	callText(t, session, "write_file", map[string]any{"path": "dir/file.txt", "content": "hello"})
	callText(t, session, "shell", map[string]any{"command": "mkdir -p dir/sub"})

	got := callText(t, session, "list_dir", map[string]any{"path": "dir"})
	if !strings.Contains(got, "file.txt\t5") {
		t.Errorf("list_dir = %q, want a %q entry with its size", got, "file.txt\t5")
	}
	if !strings.Contains(got, "sub/") {
		t.Errorf("list_dir = %q, want the subdirectory %q with a trailing slash", got, "sub/")
	}
}

// startProcess starts a background process and returns its id.
func startProcess(t *testing.T, session *mcp.ClientSession, command string) string {
	t.Helper()
	got := callText(t, session, "start_process", map[string]any{"command": command})
	id, ok := strings.CutPrefix(got, "started process ")
	if !ok || id == "" {
		t.Fatalf("start_process = %q, want %q followed by an id", got, "started process ")
	}
	return id
}

// checkUntil polls check_process until pred(accumulated) holds and
// returns the accumulated text, failing the test at the deadline.
func checkUntil(t *testing.T, session *mcp.ClientSession, id, what string, pred func(all string) bool) string {
	t.Helper()
	deadline := time.Now().Add(pollDeadline)
	var all string
	for !pred(all) {
		if time.Now().After(deadline) {
			t.Fatalf("check_process(%s): gave up waiting for %s; output so far:\n%s", id, what, all)
		}
		all += callText(t, session, "check_process", map[string]any{"process_id": id, "wait_ms": 1000})
	}
	return all
}

// TestBackgroundProcess walks the long-running-operation CUJ: start a
// staged command, poll its output incrementally, stop it, and watch it
// exit.
func TestBackgroundProcess(t *testing.T) {
	session := newSession(t)

	id := startProcess(t, session, "echo one; sleep 0.3; echo two; sleep 30")

	// Poll until both stages have appeared. The trailing sleep 30 keeps
	// the process alive until we stop it, so every poll in this loop
	// must report it still running.
	deadline := time.Now().Add(pollDeadline)
	var all string
	for !strings.Contains(all, "two") {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for staged output; got so far:\n%s", all)
		}
		chunk := callText(t, session, "check_process", map[string]any{"process_id": id, "wait_ms": 1000})
		if !strings.Contains(chunk, "[still running]") {
			t.Fatalf("check_process before stop = %q, want %q", chunk, "[still running]")
		}
		all += chunk
	}

	// check_process must return only NEW output: across every poll,
	// each stage appeared exactly once — "one" was never repeated after
	// its first return.
	if n := strings.Count(all, "one"); n != 1 {
		t.Errorf("%q appeared %d times across polls, want exactly 1; output:\n%s", "one", n, all)
	}
	if n := strings.Count(all, "two"); n != 1 {
		t.Errorf("%q appeared %d times across polls, want exactly 1; output:\n%s", "two", n, all)
	}

	stopped := callText(t, session, "stop_process", map[string]any{"process_id": id})
	if !strings.Contains(stopped, "sent signal 15") {
		t.Errorf("stop_process = %q, want the default %q", stopped, "sent signal 15")
	}

	checkUntil(t, session, id, "the process to exit", func(all string) bool {
		return strings.Contains(all, "[exited")
	})
}

func TestWriteStdin(t *testing.T) {
	session := newSession(t)

	id := startProcess(t, session, "cat")

	if got := callText(t, session, "write_stdin", map[string]any{"process_id": id, "data": "ping\n"}); got != "ok" {
		t.Errorf("write_stdin = %q, want %q", got, "ok")
	}
	checkUntil(t, session, id, "cat to echo the stdin write", func(all string) bool {
		return strings.Contains(all, "ping")
	})

	// Closing stdin ends cat cleanly.
	callText(t, session, "write_stdin", map[string]any{"process_id": id, "data": "", "close_stdin": true})
	checkUntil(t, session, id, "cat to exit after stdin closed", func(all string) bool {
		return strings.Contains(all, "[exited with code 0]")
	})
}
