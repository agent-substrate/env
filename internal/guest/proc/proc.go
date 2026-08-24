// Package proc runs commands inside the environment: bounded commands
// via Exec, background processes via Table.
//
// The table, its records, and the bounded output buffers are ordinary
// heap state. Substrate's checkpoint snapshots the whole environment —
// this memory together with the processes themselves — so everything
// survives suspend/resume with no persistence code at all. Clients hold
// process ids and byte offsets, never connections; polling by offset
// resumes cleanly no matter how many connections came and went.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// DefaultMaxBufferBytes is the per-stream cap on buffered output.
// When a stream exceeds it, the oldest bytes are dropped.
const DefaultMaxBufferBytes = 1 << 20 // 1 MiB

// defaultShell runs every command line.
const defaultShell = "/bin/sh"

// errExecTimeout marks a context deadline as Exec's own timeout rather
// than the caller's.
var errExecTimeout = errors.New("exec timeout")

// killGracePeriod is how long an exiting process group has to release
// the output pipes before its pumps stop waiting on them.
const killGracePeriod = 2 * time.Second

// Options describes a command to run.
type Options struct {
	// Command is the shell command line, run via /bin/sh -c. Required.
	Command string

	// Dir is the working directory. Defaults to the caller's.
	Dir string

	// Env holds environment variables set on top of the process's own.
	Env map[string]string
}

// ExecResult is the outcome of a bounded Exec. Err reports in-band that
// the command did not run to an exit code of its own (failed to start,
// timed out, or was killed by a signal); ExitCode is meaningless then.
type ExecResult struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	ExitCode        int
	Err             string
}

// Exec runs a command to completion and captures up to maxBuffer bytes
// of each output stream (DefaultMaxBufferBytes when 0), keeping the
// newest bytes. The returned error reports only invalid arguments; how
// the command fared is in the ExecResult.
func Exec(ctx context.Context, opts Options, stdin []byte, timeout time.Duration, maxBuffer int) (*ExecResult, error) {
	if opts.Command == "" {
		return nil, errors.New("command is required")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, errExecTimeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, defaultShell, "-c", opts.Command)
	cmd.Dir = opts.Dir
	cmd.Env = mergedEnv(opts.Env)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr buffer
	stdout.max = bufferMax(maxBuffer)
	stderr.max = bufferMax(maxBuffer)
	cmd.Stdout = bufWriter{&stdout}
	cmd.Stderr = bufWriter{&stderr}

	// Run in its own process group so cancellation kills the whole tree
	// rather than just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killGracePeriod

	err := cmd.Run()
	res := &ExecResult{
		Stdout:          stdout.data,
		Stderr:          stderr.data,
		StdoutTruncated: stdout.start > 0,
		StderrTruncated: stderr.start > 0,
	}
	// A command that reached an exit code of its own is reported by
	// that code even when the timeout fired moments after it finished —
	// the timeout label is reserved for commands the timeout actually
	// cut short.
	switch {
	case err == nil:
	case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0:
		res.ExitCode = cmd.ProcessState.ExitCode()
	case errors.Is(context.Cause(ctx), errExecTimeout):
		res.Err = fmt.Sprintf("timed out after %s", timeout)
	case cmd.ProcessState != nil:
		res.Err = cmd.ProcessState.String() // e.g. "signal: killed"
	default:
		res.Err = fmt.Sprintf("failed to start: %v", err)
	}
	return res, nil
}

// Table is the background process table. Processes are never removed:
// exited records stay until the environment goes away, so a client can
// always finish reading output it has an id for.
type Table struct {
	// MaxBufferBytes caps each process's per-stream output buffer.
	// Defaults to DefaultMaxBufferBytes. Set before the first Start.
	MaxBufferBytes int

	mu    sync.Mutex
	procs map[string]*Process
}

// NewTable returns an empty table.
func NewTable() *Table {
	return &Table{procs: make(map[string]*Process)}
}

// Start launches a background process. The returned error reports
// invalid arguments or a spawn failure; either way no process was
// registered and the same call can be retried.
func (t *Table) Start(opts Options) (*Process, error) {
	if opts.Command == "" {
		return nil, errors.New("command is required")
	}

	cmd := exec.Command(defaultShell, "-c", opts.Command)
	cmd.Dir = opts.Dir
	cmd.Env = mergedEnv(opts.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Bound the wait for the output pipes once the process exits, so a
	// backgrounded grandchild holding them open cannot wedge the record
	// in a running state forever.
	cmd.WaitDelay = killGracePeriod

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	p := &Process{
		id:        uuid.NewString(),
		command:   opts.Command,
		startTime: time.Now(),
		cmd:       cmd,
		stdin:     stdin,
		change:    make(chan struct{}),
	}
	p.stdout.max = bufferMax(t.MaxBufferBytes)
	p.stderr.max = bufferMax(t.MaxBufferBytes)
	cmd.Stdout = procWriter{p, &p.stdout}
	cmd.Stderr = procWriter{p, &p.stderr}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go p.reap()

	t.mu.Lock()
	t.procs[p.id] = p
	t.mu.Unlock()
	return p, nil
}

// Get returns the process with the given id.
func (t *Table) Get(id string) (*Process, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.procs[id]
	return p, ok
}

// List returns a snapshot of every process, running and exited.
func (t *Table) List() []Info {
	t.mu.Lock()
	procs := make([]*Process, 0, len(t.procs))
	for _, p := range t.procs {
		procs = append(procs, p)
	}
	t.mu.Unlock()

	infos := make([]Info, len(procs))
	for i, p := range procs {
		infos[i] = p.Info()
	}
	return infos
}

// Process is one background process.
type Process struct {
	id        string
	command   string
	startTime time.Time
	cmd       *exec.Cmd

	stdinMu sync.Mutex
	stdin   io.WriteCloser

	mu             sync.Mutex
	stdout, stderr buffer
	exited         *ExitStatus
	change         chan struct{} // closed and replaced on every state change
}

// ExitStatus reports how a process ended. Err is set when it did not
// run to an exit code of its own; Code is meaningless then.
type ExitStatus struct {
	Code int
	Err  string
	Time time.Time
}

// Info is a point-in-time snapshot of a process. StdoutLen and
// StderrLen are the total bytes each stream has produced — the offsets
// at the end of the output.
type Info struct {
	ID        string
	Command   string
	StartTime time.Time
	Exited    *ExitStatus
	StdoutLen int64
	StderrLen int64
}

// Output is a run of output bytes; Offset is the absolute stream offset
// of Data[0]. It can be later than a Read asked for when older bytes
// were dropped from the bounded buffer.
type Output struct {
	Data   []byte
	Offset int64
}

// ID returns the process's identifier.
func (p *Process) ID() string { return p.id }

// Info returns a snapshot of the process's state.
func (p *Process) Info() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.infoLocked()
}

func (p *Process) infoLocked() Info {
	info := Info{
		ID:        p.id,
		Command:   p.command,
		StartTime: p.startTime,
		StdoutLen: p.stdout.end(),
		StderrLen: p.stderr.end(),
	}
	if p.exited != nil {
		e := *p.exited
		info.Exited = &e
	}
	return info
}

// Read returns the process's state and any buffered output at or past
// the given offsets. When there is none and the process is still
// running, it waits up to wait for new output or exit — the bounded
// long-poll behind the data plane's Get. It returns early when ctx is
// done.
func (p *Process) Read(ctx context.Context, stdoutOff, stderrOff int64, wait time.Duration) (Info, Output, Output) {
	deadline := time.Now().Add(wait)
	p.mu.Lock()
	for p.exited == nil && p.stdout.end() <= stdoutOff && p.stderr.end() <= stderrOff {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		change := p.change
		p.mu.Unlock()

		timer := time.NewTimer(remaining)
		select {
		case <-change:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()

		p.mu.Lock()
		if ctx.Err() != nil {
			break
		}
	}
	defer p.mu.Unlock()

	info := p.infoLocked()
	stdoutData, stdoutAt := p.stdout.read(stdoutOff)
	stderrData, stderrAt := p.stderr.read(stderrOff)
	return info, Output{Data: stdoutData, Offset: stdoutAt}, Output{Data: stderrData, Offset: stderrAt}
}

// WriteStdin appends data to the process's standard input, closing it
// afterwards when closeStdin is set. A write to a full pipe blocks
// until the process reads or exits; when ctx ends first, WriteStdin
// returns ctx's error while the write completes in the background —
// delivery is at least once, so callers must not blindly resend after
// a deadline.
func (p *Process) WriteStdin(ctx context.Context, data []byte, closeStdin bool) error {
	done := make(chan error, 1)
	go func() {
		p.stdinMu.Lock()
		defer p.stdinMu.Unlock()
		if p.stdin == nil {
			done <- errors.New("stdin is closed")
			return
		}
		if _, err := p.stdin.Write(data); err != nil {
			done <- err
			return
		}
		if closeStdin {
			err := p.stdin.Close()
			p.stdin = nil
			done <- err
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Signal delivers sig to the process's process group. Signaling an
// exited process is an error, though a process exiting concurrently
// can still be seen as live for an instant — an inherent property of
// signaling by pid.
func (p *Process) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	exited := p.exited != nil
	p.mu.Unlock()
	if exited {
		return errors.New("process has exited")
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil {
		return p.cmd.Process.Signal(sig)
	}
	return nil
}

// reap waits for the process and its output, collects the exit status,
// and wakes every poller.
func (p *Process) reap() {
	err := p.cmd.Wait()
	if errors.Is(err, exec.ErrWaitDelay) {
		// The process exited cleanly; only its output pipes were held
		// open past the grace period and force-closed.
		err = nil
	}

	// Publish the exit before any other cleanup: Wait has released the
	// pid, and the exited flag is what keeps Signal from targeting it.
	exited := &ExitStatus{Time: time.Now()}
	switch {
	case err == nil:
	case p.cmd.ProcessState != nil && p.cmd.ProcessState.ExitCode() >= 0:
		exited.Code = p.cmd.ProcessState.ExitCode()
	case p.cmd.ProcessState != nil:
		exited.Err = p.cmd.ProcessState.String() // e.g. "signal: killed"
	default:
		exited.Err = err.Error()
	}
	p.mu.Lock()
	p.exited = exited
	p.broadcastLocked()
	p.mu.Unlock()

	p.stdinMu.Lock()
	if p.stdin != nil {
		p.stdin.Close()
		p.stdin = nil
	}
	p.stdinMu.Unlock()
}

// broadcastLocked wakes every Read waiting on p. The caller holds p.mu.
func (p *Process) broadcastLocked() {
	close(p.change)
	p.change = make(chan struct{})
}

// buffer accumulates one output stream, keeping at most max bytes and
// dropping the oldest beyond that. Offsets are absolute: start is the
// offset of data[0], start+len(data) the end of everything produced.
type buffer struct {
	max   int
	start int64
	data  []byte
}

func (b *buffer) write(p []byte) {
	b.data = append(b.data, p...)
	if over := len(b.data) - b.max; over > 0 {
		b.data = b.data[over:]
		b.start += int64(over)
	}
}

// read returns a copy of the buffered bytes at or past off, and the
// absolute offset of the first byte returned.
func (b *buffer) read(off int64) ([]byte, int64) {
	if off < b.start {
		off = b.start
	}
	if off >= b.end() {
		return nil, off
	}
	out := make([]byte, b.end()-off)
	copy(out, b.data[off-b.start:])
	return out, off
}

// end returns the total number of bytes the stream has produced.
func (b *buffer) end() int64 { return b.start + int64(len(b.data)) }

// bufWriter adapts a buffer to io.Writer for Exec's capture, where the
// buffer is only written from one goroutine.
type bufWriter struct{ b *buffer }

func (w bufWriter) Write(p []byte) (int, error) {
	w.b.write(p)
	return len(p), nil
}

// procWriter feeds one output stream of a background process into its
// buffer and wakes pollers. exec.Cmd calls it from the goroutine that
// drains the corresponding pipe.
type procWriter struct {
	p   *Process
	buf *buffer
}

func (w procWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	w.buf.write(b)
	w.p.broadcastLocked()
	w.p.mu.Unlock()
	return len(b), nil
}

func bufferMax(max int) int {
	if max > 0 {
		return max
	}
	return DefaultMaxBufferBytes
}

// mergedEnv layers overrides on top of the current environment.
func mergedEnv(overrides map[string]string) []string {
	env := os.Environ()
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
