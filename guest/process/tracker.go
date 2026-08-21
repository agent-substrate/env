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
	"sync"
	"syscall"
	"time"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// TrackerConfig holds resource management and isolation options for the process tracker.
type TrackerConfig struct {
	// LogDir is the directory where process stdout/stderr logs are stored.
	LogDir string
	// MaxConcurrentProcesses limits simultaneous active running commands. 0 means unlimited.
	MaxConcurrentProcesses int
	// MaxLogBytes caps stdout and stderr logs per command. 0 means unlimited.
	MaxLogBytes int64
	// DefaultProcessTimeout is the maximum duration a process is allowed to run before being killed.
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

// ProcessState tracks the execution state and log handles of a process.
type ProcessState struct {
	mu         sync.RWMutex
	ProcessID  string
	Command    []string
	Cmd        *exec.Cmd
	Status     ateenvv1.ProcessStatus
	ExitCode   int32
	StartedAt  time.Time
	FinishedAt time.Time

	StdoutPath string
	StderrPath string

	doneChan chan struct{}
	timer    *time.Timer
}

// Tracker manages process lifecycles, resource limits, and log cleanup.
type Tracker struct {
	mu              sync.RWMutex
	config          TrackerConfig
	processes       map[string]*ProcessState
	activeProcesses int
	stopPruner      chan struct{}
}

// NewTracker creates a new Process Tracker with Layer 1 resource controls.
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

	// Start background pruner routine
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

// cappedWriter limits total bytes written and appends a warning when exceeded.
type cappedWriter struct {
	w       io.Writer
	limit   int64
	written int64
	warned  bool
}

func (cw *cappedWriter) Write(p []byte) (n int, err error) {
	if cw.limit <= 0 {
		return cw.w.Write(p)
	}
	if cw.written >= cw.limit {
		if !cw.warned {
			cw.warned = true
			_, _ = cw.w.Write([]byte(fmt.Sprintf("\n\n[guest: maximum log limit of %d MB exceeded; remaining output truncated]\n", cw.limit/(1024*1024))))
		}
		return len(p), nil
	}

	remaining := cw.limit - cw.written
	toWrite := p
	if int64(len(p)) > remaining {
		toWrite = p[:remaining]
	}

	n, err = cw.w.Write(toWrite)
	cw.written += int64(n)
	if int64(len(p)) > remaining && !cw.warned {
		cw.warned = true
		_, _ = cw.w.Write([]byte(fmt.Sprintf("\n\n[guest: maximum log limit of %d MB exceeded; remaining output truncated]\n", cw.limit/(1024*1024))))
	}
	return len(p), err
}

// Start launches a new background process, enforcing concurrency and resource limits.
func (t *Tracker) Start(command []string, cwd string, env map[string]string) (*ProcessState, error) {
	if len(command) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command list cannot be empty")
	}

	t.mu.Lock()
	if t.config.MaxConcurrentProcesses > 0 && t.activeProcesses >= t.config.MaxConcurrentProcesses {
		t.mu.Unlock()
		return nil, status.Errorf(codes.ResourceExhausted,
			"maximum concurrent processes limit (%d) reached; please wait for running processes to complete or kill them",
			t.config.MaxConcurrentProcesses)
	}
	t.activeProcesses++
	t.mu.Unlock()

	processID := t.generateUniqueID()
	stdoutPath := filepath.Join(t.config.LogDir, fmt.Sprintf("%s.stdout", processID))
	stderrPath := filepath.Join(t.config.LogDir, fmt.Sprintf("%s.stderr", processID))

	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.decrementActive()
		return nil, status.Errorf(codes.Internal, "creating stdout log: %v", err)
	}

	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		stdoutFile.Close()
		t.decrementActive()
		return nil, status.Errorf(codes.Internal, "creating stderr log: %v", err)
	}

	cmd := exec.Command(command[0], command[1:]...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Wrap output writers with byte limiters to prevent disk fill-up
	cmd.Stdout = &cappedWriter{w: stdoutFile, limit: t.config.MaxLogBytes}
	cmd.Stderr = &cappedWriter{w: stderrFile, limit: t.config.MaxLogBytes}

	// Assign independent Linux Process Group ID (PGID) to kill all child subprocesses cleanly
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		stdoutFile.Close()
		stderrFile.Close()
		t.decrementActive()
		return nil, status.Errorf(codes.Internal, "starting process: %v", err)
	}

	state := &ProcessState{
		ProcessID:  processID,
		Command:    command,
		Cmd:        cmd,
		Status:     ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING,
		StartedAt:  startedAt,
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
		doneChan:   make(chan struct{}),
	}

	// Watchdog timeout to prevent runaway processes
	if t.config.DefaultProcessTimeout > 0 {
		state.timer = time.AfterFunc(t.config.DefaultProcessTimeout, func() {
			_, _ = t.Kill(processID)
		})
	}

	t.mu.Lock()
	t.processes[processID] = state
	t.mu.Unlock()

	// Background reaper goroutine
	go func() {
		waitErr := cmd.Wait()
		finishedAt := time.Now()

		state.mu.Lock()
		if state.timer != nil {
			state.timer.Stop()
		}
		state.FinishedAt = finishedAt
		_ = stdoutFile.Sync()
		_ = stderrFile.Sync()
		_ = stdoutFile.Close()
		_ = stderrFile.Close()

		if state.Status == ateenvv1.ProcessStatus_PROCESS_STATUS_TERMINATED {
			// Already marked as terminated; preserve or refine exit code from wait status if signaled
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					state.ExitCode = 128 + int32(ws.Signal())
				}
			}
		} else if waitErr == nil {
			state.Status = ateenvv1.ProcessStatus_PROCESS_STATUS_COMPLETED
			state.ExitCode = 0
		} else {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					state.Status = ateenvv1.ProcessStatus_PROCESS_STATUS_TERMINATED
					state.ExitCode = 128 + int32(ws.Signal())
				} else if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Exited() {
					state.Status = ateenvv1.ProcessStatus_PROCESS_STATUS_FAILED
					state.ExitCode = int32(ws.ExitStatus())
				} else {
					state.Status = ateenvv1.ProcessStatus_PROCESS_STATUS_FAILED
					state.ExitCode = int32(exitErr.ExitCode())
				}
			} else {
				state.Status = ateenvv1.ProcessStatus_PROCESS_STATUS_FAILED
				state.ExitCode = -1
			}
		}
		close(state.doneChan)
		state.mu.Unlock()

		t.decrementActive()
	}()

	return state, nil
}

func (t *Tracker) decrementActive() {
	t.mu.Lock()
	if t.activeProcesses > 0 {
		t.activeProcesses--
	}
	t.mu.Unlock()
}

// Get returns the process state for the given process ID.
func (t *Tracker) Get(processID string) (*ProcessState, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.processes[processID]
	return p, ok
}

// Kill terminates a running process and its process tree.
func (t *Tracker) Kill(processID string) (int32, error) {
	t.mu.RLock()
	state, ok := t.processes[processID]
	t.mu.RUnlock()

	if !ok {
		return 0, status.Errorf(codes.NotFound, "process %q not found", processID)
	}

	state.mu.Lock()
	if state.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING {
		exitCode := state.ExitCode
		state.mu.Unlock()
		return exitCode, nil
	}

	state.Status = ateenvv1.ProcessStatus_PROCESS_STATUS_TERMINATED
	state.ExitCode = 128 + int32(syscall.SIGKILL) // 137
	if state.timer != nil {
		state.timer.Stop()
	}
	pid := state.Cmd.Process.Pid
	state.mu.Unlock()

	// Send SIGKILL to the entire process group (-PID)
	_ = syscall.Kill(-pid, syscall.SIGKILL)

	// Wait for reaper goroutine
	select {
	case <-state.doneChan:
	case <-time.After(2 * time.Second):
	}

	state.mu.RLock()
	exitCode := state.ExitCode
	state.mu.RUnlock()

	return exitCode, nil
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

// pruneExpired cleans up completed processes older than RetentionPeriod.
func (t *Tracker) pruneExpired() {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	for id, state := range t.processes {
		state.mu.RLock()
		isDone := state.Status != ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING
		finishedAt := state.FinishedAt
		state.mu.RUnlock()

		if isDone && !finishedAt.IsZero() && t.config.RetentionPeriod > 0 {
			if now.Sub(finishedAt) > t.config.RetentionPeriod {
				_ = os.Remove(state.StdoutPath)
				_ = os.Remove(state.StderrPath)
				delete(t.processes, id)
			}
		}
	}
}

// ToProto converts a ProcessState to the protobuf Process message.
func (p *ProcessState) ToProto() *ateenvv1.Process {
	p.mu.RLock()
	defer p.mu.RUnlock()

	proto := &ateenvv1.Process{
		ProcessId: p.ProcessID,
		Status:    p.Status,
		ExitCode:  p.ExitCode,
		StartedAt: timestamppb.New(p.StartedAt),
	}
	if !p.FinishedAt.IsZero() {
		proto.FinishedAt = timestamppb.New(p.FinishedAt)
	}
	return proto
}

// ReadLogs reads log bytes from a log file at a specific byte offset.
func ReadLogs(filePath string, offset int64) ([]byte, int64, error) {
	f, err := os.Open(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	if offset >= size {
		return nil, size, nil
	}

	length := size - offset
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	return buf[:n], size, nil
}
