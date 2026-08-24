package proc_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/guest/proc"
)

// farOffset is past any offset these tests produce, so a Read using it
// long-polls until exit (or its wait elapses) without returning data.
const farOffset = int64(1) << 62

// numberedLines returns n lines "00000\n", "00001\n", ... (6 bytes each).
func numberedLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%05d\n", i)
	}
	return b.String()
}

// countLoop is a shell loop that prints numberedLines(n) to stdout.
func countLoop(n int) string {
	return fmt.Sprintf(`i=0; while [ $i -lt %d ]; do printf '%%05d\n' $i; i=$((i+1)); done`, n)
}

// waitExit long-polls p until it reports an exit status.
func waitExit(t *testing.T, p *proc.Process) proc.Info {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		info, _, _ := p.Read(t.Context(), farOffset, farOffset, time.Second)
		if info.Exited != nil {
			return info
		}
	}
	t.Fatalf("process %s did not exit within deadline", p.ID())
	return proc.Info{}
}

// readStdoutUntil chains Reads from off, accumulating stdout until it
// contains want. It returns the accumulated bytes and the next offset.
func readStdoutUntil(t *testing.T, p *proc.Process, off int64, want string) (string, int64) {
	t.Helper()
	var acc strings.Builder
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		info, out, _ := p.Read(t.Context(), off, farOffset, time.Second)
		if len(out.Data) > 0 {
			if out.Offset != off {
				t.Fatalf("stdout offset jumped: got %d, want %d", out.Offset, off)
			}
			acc.Write(out.Data)
			off += int64(len(out.Data))
		}
		if strings.Contains(acc.String(), want) {
			return acc.String(), off
		}
		if info.Exited != nil && off >= info.StdoutLen {
			t.Fatalf("process exited; stdout %q never contained %q", acc.String(), want)
		}
	}
	t.Fatalf("timed out waiting for stdout to contain %q; got %q", want, acc.String())
	return "", 0
}

func TestExecSeparatesStdoutAndStderr(t *testing.T) {
	res, err := proc.Exec(t.Context(), proc.Options{Command: "echo out; echo err >&2"}, nil, 0, 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := string(res.Stdout); got != "out\n" {
		t.Errorf("stdout = %q, want %q", got, "out\n")
	}
	if got := string(res.Stderr); got != "err\n" {
		t.Errorf("stderr = %q, want %q", got, "err\n")
	}
	if res.ExitCode != 0 || res.Err != "" {
		t.Errorf("ExitCode = %d, Err = %q, want 0 and empty", res.ExitCode, res.Err)
	}
	if res.StdoutTruncated || res.StderrTruncated {
		t.Errorf("unexpected truncation: stdout=%v stderr=%v", res.StdoutTruncated, res.StderrTruncated)
	}
}

func TestExecNonzeroExitCode(t *testing.T) {
	res, err := proc.Exec(t.Context(), proc.Options{Command: "exit 7"}, nil, 0, 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.Err != "" {
		t.Errorf("Err = %q, want empty for a command that ran to its own exit code", res.Err)
	}
}

func TestExecFeedsStdin(t *testing.T) {
	res, err := proc.Exec(t.Context(), proc.Options{Command: "cat"}, []byte("hello from stdin"), 0, 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := string(res.Stdout); got != "hello from stdin" {
		t.Errorf("stdout = %q, want %q", got, "hello from stdin")
	}
	if res.ExitCode != 0 || res.Err != "" {
		t.Errorf("ExitCode = %d, Err = %q, want 0 and empty", res.ExitCode, res.Err)
	}
}

func TestExecEnvOverride(t *testing.T) {
	opts := proc.Options{
		Command: `printf %s "$PROC_TEST_ENV"`,
		Env:     map[string]string{"PROC_TEST_ENV": "xyzzy"},
	}
	res, err := proc.Exec(t.Context(), opts, nil, 0, 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := string(res.Stdout); got != "xyzzy" {
		t.Errorf("stdout = %q, want %q", got, "xyzzy")
	}
}

func TestExecDir(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	res, err := proc.Exec(t.Context(), proc.Options{Command: "pwd -P", Dir: dir}, nil, 0, 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("Err = %q, want empty", res.Err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != resolved {
		t.Errorf("pwd -P = %q, want %q", got, resolved)
	}
}

func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	// The shell prints its own pid (which is the process group id, thanks
	// to Setpgid), backgrounds a long sleep, and blocks on another. On
	// timeout the whole group — including the backgrounded sleep — must
	// die.
	opts := proc.Options{Command: "echo $$; sleep 30 & sleep 30"}
	res, err := proc.Exec(t.Context(), opts, nil, 300*time.Millisecond, 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Err, "timed out") {
		t.Fatalf("Err = %q, want it to contain %q", res.Err, "timed out")
	}

	pgid, err := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
	if err != nil {
		t.Fatalf("parsing shell pid from stdout %q: %v", res.Stdout, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break // the whole group is gone
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still alive after timeout (kill probe: %v)", pgid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecTruncatesKeepingNewestBytes(t *testing.T) {
	const maxBuffer = 1024
	full := numberedLines(200) // 1200 bytes, more than maxBuffer
	res, err := proc.Exec(t.Context(), proc.Options{Command: countLoop(200)}, nil, 0, maxBuffer)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Err != "" || res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, Err = %q, want 0 and empty", res.ExitCode, res.Err)
	}
	if !res.StdoutTruncated {
		t.Error("StdoutTruncated = false, want true")
	}
	if res.StderrTruncated {
		t.Error("StderrTruncated = true, want false")
	}
	if got, want := string(res.Stdout), full[len(full)-maxBuffer:]; got != want {
		t.Errorf("stdout kept %d bytes ending %q, want the newest %d bytes ending %q",
			len(got), tail(got), maxBuffer, tail(want))
	}
}

// tail returns the last few bytes of s for readable failure messages.
func tail(s string) string {
	if len(s) > 24 {
		return "..." + s[len(s)-24:]
	}
	return s
}

func TestTableStartReadLoop(t *testing.T) {
	tbl := proc.NewTable()
	p, err := tbl.Start(proc.Options{Command: `echo one; sleep 0.3; echo two`})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Signal(syscall.SIGKILL) })

	// The first Read at offset 0 long-polls until "one" appears.
	_, out, _ := p.Read(t.Context(), 0, 0, 5*time.Second)
	if out.Offset != 0 {
		t.Fatalf("first Read offset = %d, want 0", out.Offset)
	}
	if !strings.HasPrefix(string(out.Data), "one\n") {
		t.Fatalf("first Read data = %q, want prefix %q", out.Data, "one\n")
	}
	acc := string(out.Data)
	off := out.Offset + int64(len(out.Data))

	// A Read at the advanced offset long-polls and returns once "two"
	// shows up after the sleep.
	if !strings.Contains(acc, "two\n") {
		more, next := readStdoutUntil(t, p, off, "two\n")
		acc += more
		off = next
	}
	if acc != "one\ntwo\n" {
		t.Fatalf("accumulated stdout = %q, want %q", acc, "one\ntwo\n")
	}

	info := waitExit(t, p)
	if info.Exited.Code != 0 || info.Exited.Err != "" {
		t.Errorf("Exited = %+v, want code 0 and empty Err", info.Exited)
	}
	if off != info.StdoutLen {
		t.Errorf("chained offset = %d, want StdoutLen %d", off, info.StdoutLen)
	}
}

func TestReadLongPollWakesOnExit(t *testing.T) {
	tbl := proc.NewTable()
	p, err := tbl.Start(proc.Options{Command: "sleep 0.3"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Signal(syscall.SIGKILL) })

	done := make(chan proc.Info, 1)
	go func() {
		info, _, _ := p.Read(t.Context(), 0, 0, 5*time.Minute)
		done <- info
	}()

	select {
	case info := <-done:
		if info.Exited == nil {
			t.Fatal("Read returned without Exited set")
		}
		if info.Exited.Code != 0 || info.Exited.Err != "" {
			t.Errorf("Exited = %+v, want code 0 and empty Err", info.Exited)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Read with a long wait did not wake when the process exited")
	}
}

func TestBoundedBuffersDropOldest(t *testing.T) {
	tbl := proc.NewTable()
	tbl.MaxBufferBytes = 64
	full := numberedLines(100) // 600 bytes, far over the 64-byte cap

	p, err := tbl.Start(proc.Options{Command: countLoop(100)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	info := waitExit(t, p)
	if info.Exited.Code != 0 || info.Exited.Err != "" {
		t.Fatalf("Exited = %+v, want code 0 and empty Err", info.Exited)
	}
	if info.StdoutLen != int64(len(full)) {
		t.Fatalf("StdoutLen = %d, want %d", info.StdoutLen, len(full))
	}

	_, out, _ := p.Read(t.Context(), 0, 0, 0)
	if out.Offset <= 0 {
		t.Fatalf("Read at 0 returned Offset %d, want > 0 after old bytes were dropped", out.Offset)
	}
	if want := int64(len(full) - 64); out.Offset != want {
		t.Errorf("Offset = %d, want %d", out.Offset, want)
	}
	if got, want := string(out.Data), full[len(full)-64:]; got != want {
		t.Errorf("Data = %q, want newest bytes %q", got, want)
	}
	if out.Offset+int64(len(out.Data)) != info.StdoutLen {
		t.Errorf("Offset + len(Data) = %d, want StdoutLen %d",
			out.Offset+int64(len(out.Data)), info.StdoutLen)
	}
}

func TestWriteStdin(t *testing.T) {
	tbl := proc.NewTable()
	p, err := tbl.Start(proc.Options{Command: "cat"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Signal(syscall.SIGKILL) })

	if err := p.WriteStdin(t.Context(), []byte("hello\n"), false); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}
	acc, off := readStdoutUntil(t, p, 0, "hello\n")

	if err := p.WriteStdin(t.Context(), []byte("world\n"), true); err != nil {
		t.Fatalf("WriteStdin with close: %v", err)
	}
	info := waitExit(t, p)
	if info.Exited.Code != 0 || info.Exited.Err != "" {
		t.Errorf("Exited = %+v, want code 0 and empty Err after stdin close", info.Exited)
	}

	more, _ := readStdoutUntil(t, p, off, "world\n")
	if got := acc + more; got != "hello\nworld\n" {
		t.Errorf("stdout = %q, want %q", got, "hello\nworld\n")
	}

	if err := p.WriteStdin(t.Context(), []byte("late"), false); err == nil {
		t.Error("WriteStdin after close succeeded, want error")
	}
}

func TestSignalKillsProcess(t *testing.T) {
	tbl := proc.NewTable()
	p, err := tbl.Start(proc.Options{Command: "sleep 60"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Signal(syscall.SIGKILL) })

	if err := p.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Signal(SIGKILL): %v", err)
	}
	info := waitExit(t, p)
	errText := strings.ToLower(info.Exited.Err)
	if !strings.Contains(errText, "signal") && !strings.Contains(errText, "kill") {
		t.Errorf("Exited.Err = %q, want it to mention the signal/kill", info.Exited.Err)
	}

	if err := p.Signal(syscall.SIGTERM); err == nil {
		t.Error("Signal after exit succeeded, want error")
	}
}

func TestTableListGetAndStartErrors(t *testing.T) {
	tbl := proc.NewTable()
	p1, err := tbl.Start(proc.Options{Command: "echo a"})
	if err != nil {
		t.Fatalf("Start p1: %v", err)
	}
	p2, err := tbl.Start(proc.Options{Command: "echo b"})
	if err != nil {
		t.Fatalf("Start p2: %v", err)
	}

	infos := tbl.List()
	if len(infos) != 2 {
		t.Fatalf("List returned %d processes, want 2", len(infos))
	}
	byID := make(map[string]proc.Info, len(infos))
	for _, info := range infos {
		byID[info.ID] = info
	}
	if info, ok := byID[p1.ID()]; !ok || info.Command != "echo a" {
		t.Errorf("List missing p1 %s or wrong command: %+v", p1.ID(), info)
	}
	if info, ok := byID[p2.ID()]; !ok || info.Command != "echo b" {
		t.Errorf("List missing p2 %s or wrong command: %+v", p2.ID(), info)
	}

	if got, ok := tbl.Get(p1.ID()); !ok || got.ID() != p1.ID() {
		t.Errorf("Get(%q) = %v, %v; want p1, true", p1.ID(), got, ok)
	}
	if _, ok := tbl.Get("no-such-id"); ok {
		t.Error("Get of unknown id returned ok=true, want false")
	}

	if _, err := tbl.Start(proc.Options{}); err == nil {
		t.Error("Start with empty command succeeded, want error")
	}
	badDir := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := tbl.Start(proc.Options{Command: "true", Dir: badDir}); err == nil {
		t.Error("Start with nonexistent Dir succeeded, want error")
	}
	if got := len(tbl.List()); got != 2 {
		t.Errorf("List returned %d processes after failed Starts, want 2 (nothing registered)", got)
	}
}
