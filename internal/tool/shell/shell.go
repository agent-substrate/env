package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agent-substrate/sandbox/internal/tool"
)

const (
	defaultShell = "/bin/sh"
	shellFlag    = "-c"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

// Config configures the shell tool.
type Config struct {
	Shell          string
	Workdir        string
	Timeout        time.Duration
	MaxTimeout     time.Duration
	MaxOutputBytes int
	Allow          []string
	Env            []string
}

func (c Config) withDefaults() Config {
	if c.Shell == "" {
		c.Shell = defaultShell
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxTimeout <= 0 {
		c.MaxTimeout = 10 * time.Minute
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 64 * 1024
	}
	return c
}

type shellParams struct {
	Command        string `json:"command"`
	Workdir        string `json:"workdir"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// New returns the shell command tool, with working directories confined to sb.
func New(sb *tool.Sandbox, cfg Config) tool.Tool {
	cfg = cfg.withDefaults()

	desc := "Run a shell command in the workspace and return its stdout, stderr, and exit " +
		"code. Commands run through " + cfg.Shell + " with a timeout, so avoid interactive " +
		"programs and long-running servers — they will be killed. Prefer the dedicated " +
		"read_file, glob, and grep tools where they fit; they give structured output and " +
		"cannot be chained into unintended side effects."
	if len(cfg.Allow) > 0 {
		desc += " Only these commands are permitted: " + strings.Join(cfg.Allow, ", ") + "."
	}

	def := tool.ToolDefinition{
		Name:        "shell",
		Description: desc,
		Parameters: tool.Object([]string{"command"}, map[string]tool.Property{
			"command":         tool.String("Command line to execute."),
			"workdir":         tool.String("Working directory, relative to the workspace root. Defaults to the workspace root."),
			"timeout_seconds": tool.Integer(fmt.Sprintf("Timeout in seconds (default %d, max %d).", int(cfg.Timeout.Seconds()), int(cfg.MaxTimeout.Seconds()))),
		}),
	}

	return tool.New(def, func(ctx context.Context, p shellParams) (string, error) {
		command := strings.TrimSpace(p.Command)
		if command == "" {
			return "", fmt.Errorf("command must not be empty")
		}
		if err := cfg.permit(command); err != nil {
			return "", err
		}

		dir := cfg.Workdir
		if p.Workdir != "" {
			dir = p.Workdir
		}
		abs, err := sb.Resolve(dir)
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return "", fmt.Errorf("workdir %s is not a directory", sb.Rel(abs))
		}

		timeout := cfg.Timeout
		if p.TimeoutSeconds > 0 {
			timeout = time.Duration(p.TimeoutSeconds) * time.Second
		}
		if timeout > cfg.MaxTimeout {
			timeout = cfg.MaxTimeout
		}

		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, cfg.Shell, shellFlag, command)
		cmd.Dir = abs
		cmd.Env = cfg.Env
		cmd.Stdin = nil
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		setProcessGroup(cmd)
		cmd.Cancel = func() error { return killProcessGroup(cmd) }
		cmd.WaitDelay = 2 * time.Second

		start := time.Now()
		runErr := cmd.Run()
		elapsed := time.Since(start).Round(time.Millisecond)

		var out strings.Builder
		fmt.Fprintf(&out, "$ %s\n", command)
		exitCode := cmd.ProcessState.ExitCode()
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)

		switch {
		case timedOut:
			fmt.Fprintf(&out, "timed out after %s (process group killed)\n", timeout)
		case runErr != nil && exitCode < 0:
			fmt.Fprintf(&out, "failed to run: %v\n", runErr)
		default:
			fmt.Fprintf(&out, "exit code: %d (%s)\n", exitCode, elapsed)
		}
		writeStream(&out, "stdout", stdout.String(), cfg.MaxOutputBytes)
		writeStream(&out, "stderr", stderr.String(), cfg.MaxOutputBytes)

		return out.String(), nil
	})
}

var shellMetachars = []string{"&&", "||", ";", "|", "`", "$(", ">", "<", "\n"}

func (c Config) permit(command string) error {
	if len(c.Allow) == 0 {
		return nil
	}
	for _, meta := range shellMetachars {
		if strings.Contains(command, meta) {
			return fmt.Errorf("shell operator %q is not permitted; run one command per call", meta)
		}
	}
	head := strings.Fields(command)[0]
	if slices.Contains(c.Allow, head) {
		return nil
	}
	return fmt.Errorf("command %q is not permitted; allowed commands: %s", head, strings.Join(c.Allow, ", "))
}

func writeStream(out *strings.Builder, name, body string, max int) {
	if body == "" {
		return
	}
	truncated := false
	if len(body) > max {
		body, truncated = body[:max], true
	}
	fmt.Fprintf(out, "\n--- %s ---\n%s", name, body)
	if !strings.HasSuffix(body, "\n") {
		out.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(out, "[%s truncated at %d bytes]\n", name, max)
	}
}
