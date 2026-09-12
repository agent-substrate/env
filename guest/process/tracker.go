// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package process

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// DefaultLogDir is the primary location for log spooling on disk/volume.
	DefaultLogDir = "/var/log/ate-jobs"
	// DefaultMaxConcurrentProcesses is the default limit on active running jobs.
	DefaultMaxConcurrentProcesses = 10
	// DefaultMaxLogBytes is the default maximum log size per stream (10 MB).
	DefaultMaxLogBytes int64 = 10 * 1024 * 1024
	// DefaultProcessTimeout is the default watchdog timeout for running processes (1 hour).
	DefaultProcessTimeout = 1 * time.Hour
	// DefaultRetentionPeriod is the default time after completion before pruning logs (1 hour).
	DefaultRetentionPeriod = 1 * time.Hour
	// DefaultMaxRetainedProcesses is the maximum number of completed processes kept in history.
	DefaultMaxRetainedProcesses = 100
)

// Errors returned by Tracker operations. The gRPC service maps them to
// status codes.
var (
	// ErrNotFound is returned when no process has the given ID.
	ErrNotFound = errors.New("process not found")
	// ErrExited is returned when an operation needs a running process but it has exited.
	ErrExited = errors.New("process has exited")
	// ErrNoStdin is returned when writing input to a process started without stdin.
	ErrNoStdin = errors.New("process was started without stdin")
	// ErrStdinClosed is returned when writing input after stdin has been closed.
	ErrStdinClosed = errors.New("stdin is closed")
	// ErrTooManyProcesses is returned when the concurrency limit is reached.
	ErrTooManyProcesses = errors.New("maximum concurrent processes limit reached")
)

// TrackerConfig holds resource management and isolation options for the process tracker.
type TrackerConfig struct {
	// LogDir is the directory where process stdout/stderr logs are stored.
	LogDir string
	// Workspace is the working and confinement directory for process operations.
	Workspace string
	// MaxConcurrentProcesses limits simultaneous active running commands. 0 means unlimited.
	MaxConcurrentProcesses int
	// MaxLogBytes caps stdout and stderr logs per command. 0 means unlimited.
	MaxLogBytes int64
	// DefaultProcessTimeout is the maximum duration a process is allowed to run
	// before being killed, unless the start request sets its own timeout.
	DefaultProcessTimeout time.Duration
	// RetentionPeriod is how long completed process logs are kept before being pruned.
	RetentionPeriod time.Duration
	// MaxRetainedProcesses caps total historical completed processes kept in memory.
	MaxRetainedProcesses int
}

// resolveDefaultLogDir returns /var/log/ate-jobs or falls back to os.TempDir if not writable.
func resolveDefaultLogDir() string {
	if env := os.Getenv("LOG_DIR"); env != "" {
		return env
	}
	if err := os.MkdirAll(DefaultLogDir, 0755); err == nil {
		return DefaultLogDir
	}
	return filepath.Join(os.TempDir(), "ate-jobs")
}

// DefaultConfig returns standard production settings for Tracker.
func DefaultConfig(logDir string) TrackerConfig {
	if logDir == "" {
		logDir = resolveDefaultLogDir()
	}
	return TrackerConfig{
		LogDir:                 logDir,
		MaxConcurrentProcesses: DefaultMaxConcurrentProcesses,
		MaxLogBytes:            DefaultMaxLogBytes,
		DefaultProcessTimeout:  DefaultProcessTimeout,
		RetentionPeriod:        DefaultRetentionPeriod,
		MaxRetainedProcesses:   DefaultMaxRetainedProcesses,
	}
}

// StartOptions describes a process to launch.
type StartOptions struct {
	// Command is the binary and its arguments.
	Command []string
	// Cwd is the working directory; empty means the tracker's Workspace.
	Cwd string
	// Env holds extra environment variables layered on the guest's environment.
	Env map[string]string
	// Stdin opens a pipe for standard input fed by WriteInput. When false,
	// stdin reads as empty.
	Stdin bool
	// Timeout kills the process group after this duration. Zero uses the
	// tracker's DefaultProcessTimeout.
	Timeout time.Duration
}

// ProcessState tracks the execution state and I/O handles of a process.
type ProcessState struct {
	mu         sync.RWMutex
	ProcessID  string
	Command    []string
	Pid        int
	State      ateenvv1alpha.ProcessState
	ExitCode   int32          // valid once exited and Signal == 0
	Signal     syscall.Signal // nonzero if the process was terminated by a signal
	StartedAt  time.Time
	FinishedAt time.Time

	StdoutPath string
	StderrPath string

	stdinMu     sync.Mutex
	stdin       io.WriteCloser // nil when started without stdin
	stdinClosed bool

	output *notifier
	done   chan struct{}
	timer  *time.Timer
}

// Exited reports whether the process has been reaped.
func (p *ProcessState) Exited() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.State == ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED
}

// Done returns a channel closed once the process has exited and its output
// has been flushed to the log spool.
func (p *ProcessState) Done() <-chan struct{} { return p.done }

// OutputChanged returns a channel closed the next time bytes are appended to
// either output spool. Obtain it before reading the spool to avoid missing a
// wakeup.
func (p *ProcessState) OutputChanged() <-chan struct{} { return p.output.wait() }

// ToProto converts a ProcessState to the protobuf Process message.
func (p *ProcessState) ToProto() *ateenvv1alpha.Process {
	p.mu.RLock()
	defer p.mu.RUnlock()

	proto := &ateenvv1alpha.Process{
		ProcessId: p.ProcessID,
		Command:   append([]string(nil), p.Command...),
		Pid:       int32(p.Pid),
		State:     p.State,
		StartedAt: timestamppb.New(p.StartedAt),
	}
	if p.State == ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
		proto.ExitCode = p.ExitCode
		if p.Signal != 0 {
			proto.ExitCode = 128 + int32(FromSyscallSignal(p.Signal))
		}
	}
	if !p.FinishedAt.IsZero() {
		proto.FinishedAt = timestamppb.New(p.FinishedAt)
	}
	return proto
}

// notifier broadcasts "something changed" to any number of waiters by
// closing and replacing a channel.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotifier() *notifier { return &notifier{ch: make(chan struct{})} }

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) notify() {
	n.mu.Lock()
	close(n.ch)
	n.ch = make(chan struct{})
	n.mu.Unlock()
}

// Tracker manages process lifecycles, resource limits, and log cleanup.
type Tracker struct {
	mu              sync.RWMutex
	config          TrackerConfig
	processes       map[string]*ProcessState
	activeProcesses int
	stopPruner      chan struct{}
}

// NewTracker creates a new process Tracker.
func NewTracker(cfg TrackerConfig) (*Tracker, error) {
	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(os.TempDir(), "ate-jobs")
	}
	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}

	t := &Tracker{
		config:     cfg,
		processes:  make(map[string]*ProcessState),
		stopPruner: make(chan struct{}),
	}
	go t.prunerLoop()
	return t, nil
}

// Close stops the background pruner.
func (t *Tracker) Close() {
	close(t.stopPruner)
}

// generateUniqueID generates a cryptographically random, collision-free process ID.
func (t *Tracker) generateUniqueID() string {
	b := make([]byte, 12)
	for {
		_, _ = rand.Read(b)
		id := fmt.Sprintf("proc-%s", hex.EncodeToString(b))

		t.mu.RLock()
		_, exists := t.processes[id]
		t.mu.RUnlock()

		if !exists {
			return id
		}
	}
}

// cappedWriter limits total bytes written, appends a warning when exceeded,
// and reports every write to onWrite.
type cappedWriter struct {
	w       io.Writer
	limit   int64
	written int64
	warned  bool
	onWrite func()
}

func (cw *cappedWriter) Write(p []byte) (int, error) {
	if cw.onWrite != nil {
		defer cw.onWrite()
	}
	if cw.limit <= 0 {
		return cw.w.Write(p)
	}
	if cw.written >= cw.limit {
		cw.warnOnce()
		return len(p), nil
	}

	remaining := cw.limit - cw.written
	toWrite := p
	if int64(len(p)) > remaining {
		toWrite = p[:remaining]
	}

	n, err := cw.w.Write(toWrite)
	cw.written += int64(n)
	if int64(len(p)) > remaining {
		cw.warnOnce()
	}
	return len(p), err
}

func (cw *cappedWriter) warnOnce() {
	if cw.warned {
		return
	}
	cw.warned = true
	_, _ = fmt.Fprintf(cw.w, "\n\n[guest: maximum log limit of %d MB exceeded; remaining output truncated]\n", cw.limit/(1024*1024))
}

// Start launches a new background process, enforcing concurrency and resource limits.
func (t *Tracker) Start(opts StartOptions) (*ProcessState, error) {
	if len(opts.Command) == 0 {
		return nil, errors.New("command cannot be empty")
	}

	t.mu.Lock()
	if t.config.MaxConcurrentProcesses > 0 && t.activeProcesses >= t.config.MaxConcurrentProcesses {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w (%d)", ErrTooManyProcesses, t.config.MaxConcurrentProcesses)
	}
	t.activeProcesses++
	t.mu.Unlock()

	processID := t.generateUniqueID()
	stdoutPath := filepath.Join(t.config.LogDir, processID+".stdout")
	stderrPath := filepath.Join(t.config.LogDir, processID+".stderr")

	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.decrementActive()
		return nil, fmt.Errorf("creating stdout log: %w", err)
	}
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		stdoutFile.Close()
		t.decrementActive()
		return nil, fmt.Errorf("creating stderr log: %w", err)
	}

	state := &ProcessState{
		ProcessID:  processID,
		Command:    append([]string(nil), opts.Command...),
		State:      ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING,
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
		output:     newNotifier(),
		done:       make(chan struct{}),
	}

	cmd := exec.Command(opts.Command[0], opts.Command[1:]...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	} else if t.config.Workspace != "" {
		cmd.Dir = t.config.Workspace
	}
	if len(opts.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range opts.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	// Wrap output writers with byte limiters to prevent disk fill-up and
	// wake output streamers on every write.
	cmd.Stdout = &cappedWriter{w: stdoutFile, limit: t.config.MaxLogBytes, onWrite: state.output.notify}
	cmd.Stderr = &cappedWriter{w: stderrFile, limit: t.config.MaxLogBytes, onWrite: state.output.notify}
	// Give the process its own group so signals reach the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if opts.Stdin {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			stdoutFile.Close()
			stderrFile.Close()
			t.decrementActive()
			return nil, fmt.Errorf("opening stdin pipe: %w", err)
		}
		state.stdin = stdin
	}

	state.StartedAt = time.Now()
	if err := cmd.Start(); err != nil {
		stdoutFile.Close()
		stderrFile.Close()
		t.decrementActive()
		return nil, fmt.Errorf("starting process: %w", err)
	}
	state.Pid = cmd.Process.Pid

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = t.config.DefaultProcessTimeout
	}
	if timeout > 0 {
		state.timer = time.AfterFunc(timeout, func() {
			_ = t.Signal(processID, syscall.SIGKILL)
		})
	}

	t.mu.Lock()
	t.processes[processID] = state
	t.mu.Unlock()

	go t.reap(state, cmd, stdoutFile, stderrFile)
	return state, nil
}

// reap waits for the process to exit, records its exit status, and flushes
// the output spool before signalling waiters.
func (t *Tracker) reap(state *ProcessState, cmd *exec.Cmd, stdoutFile, stderrFile *os.File) {
	_ = cmd.Wait() // Wait also closes the stdin pipe, if any.
	finishedAt := time.Now()

	// Wait returned only after the stdout/stderr copiers finished, so the
	// spool is complete once synced.
	_ = stdoutFile.Sync()
	_ = stderrFile.Sync()
	_ = stdoutFile.Close()
	_ = stderrFile.Close()

	state.mu.Lock()
	if state.timer != nil {
		state.timer.Stop()
	}
	state.FinishedAt = finishedAt
	state.State = ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED
	state.ExitCode, state.Signal = exitStatus(cmd.ProcessState)
	state.mu.Unlock()

	state.stdinMu.Lock()
	state.stdinClosed = true
	state.stdinMu.Unlock()

	close(state.done)
	state.output.notify()
	t.decrementActive()
}

// exitStatus decodes how a waited process ended: (code, 0) for a normal
// exit or (0, signal) when terminated by a signal.
func exitStatus(ps *os.ProcessState) (int32, syscall.Signal) {
	if ps == nil {
		return -1, 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 0, ws.Signal()
		}
		if ws.Exited() {
			return int32(ws.ExitStatus()), 0
		}
	}
	return int32(ps.ExitCode()), 0
}

func (t *Tracker) decrementActive() {
	t.mu.Lock()
	if t.activeProcesses > 0 {
		t.activeProcesses--
	}
	t.mu.Unlock()
}

// Get returns the process state for the given process ID.
func (t *Tracker) Get(processID string) (*ProcessState, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.processes[processID]
	if !ok {
		return nil, ErrNotFound
	}
	return p, nil
}

// Signal delivers sig to the process group of a running process.
func (t *Tracker) Signal(processID string, sig syscall.Signal) error {
	state, err := t.Get(processID)
	if err != nil {
		return err
	}

	state.mu.RLock()
	running := state.State == ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING
	pid := state.Pid
	state.mu.RUnlock()
	if !running {
		return ErrExited
	}

	// Negative PID targets the whole process group (the leader's PGID == PID
	// thanks to Setpgid).
	if err := syscall.Kill(-pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrExited
		}
		return fmt.Errorf("sending %v: %w", sig, err)
	}
	return nil
}

// WriteInput writes data to the stdin of a process started with Stdin.
func (t *Tracker) WriteInput(processID string, data []byte) (int, error) {
	state, err := t.Get(processID)
	if err != nil {
		return 0, err
	}
	if state.stdin == nil {
		return 0, ErrNoStdin
	}

	state.stdinMu.Lock()
	defer state.stdinMu.Unlock()
	if state.stdinClosed {
		if state.Exited() {
			return 0, ErrExited
		}
		return 0, ErrStdinClosed
	}
	n, err := state.stdin.Write(data)
	if err != nil {
		if state.Exited() || errors.Is(err, syscall.EPIPE) {
			return n, ErrExited
		}
		return n, fmt.Errorf("writing stdin: %w", err)
	}
	return n, nil
}

// CloseInput closes the stdin of a process, delivering EOF. It is idempotent.
func (t *Tracker) CloseInput(processID string) error {
	state, err := t.Get(processID)
	if err != nil {
		return err
	}
	if state.stdin == nil {
		return ErrNoStdin
	}

	state.stdinMu.Lock()
	defer state.stdinMu.Unlock()
	if state.stdinClosed {
		return nil
	}
	state.stdinClosed = true
	if err := state.stdin.Close(); err != nil {
		return fmt.Errorf("closing stdin: %w", err)
	}
	return nil
}

// prunerLoop periodically removes expired process states and log files.
func (t *Tracker) prunerLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopPruner:
			return
		case <-ticker.C:
			t.pruneExpired()
		}
	}
}

// pruneExpired cleans up exited processes older than RetentionPeriod and
// trims history beyond MaxRetainedProcesses, oldest first.
func (t *Tracker) pruneExpired() {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	var exited []*ProcessState
	for _, state := range t.processes {
		state.mu.RLock()
		done := state.State == ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED
		finishedAt := state.FinishedAt
		state.mu.RUnlock()
		if !done {
			continue
		}
		if t.config.RetentionPeriod > 0 && now.Sub(finishedAt) > t.config.RetentionPeriod {
			t.forget(state)
			continue
		}
		exited = append(exited, state)
	}

	if t.config.MaxRetainedProcesses > 0 && len(exited) > t.config.MaxRetainedProcesses {
		sort.Slice(exited, func(i, j int) bool { return exited[i].FinishedAt.Before(exited[j].FinishedAt) })
		for _, state := range exited[:len(exited)-t.config.MaxRetainedProcesses] {
			t.forget(state)
		}
	}
}

// forget drops a process and its spool. Caller holds t.mu.
func (t *Tracker) forget(state *ProcessState) {
	_ = os.Remove(state.StdoutPath)
	_ = os.Remove(state.StderrPath)
	delete(t.processes, state.ProcessID)
}

// ReadSpool reads bytes from a log spool file starting at offset and returns
// them with the new offset (the file size).
func ReadSpool(filePath string, offset int64) ([]byte, int64, error) {
	f, err := os.Open(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, offset, nil
		}
		return nil, offset, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	size := info.Size()
	if offset >= size {
		// Never move the cursor backwards: an offset past the end means
		// "skip everything written so far".
		return nil, offset, nil
	}

	buf := make([]byte, size-offset)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, offset, err
	}
	return buf[:n], offset + int64(n), nil
}
