package shell

import (
	"context"
	"fmt"
	"strings"
	"time"

	guestsys "github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/tool"
)

const defaultShell = "/bin/sh"

// Config configures the shell tool.
type Config struct {
	Shell          string
	Timeout        time.Duration
	MaxOutputBytes int
	Env            []string
}

func (c Config) withDefaults() Config {
	if c.Shell == "" {
		c.Shell = defaultShell
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 1 << 20 // 1 MiB
	}
	return c
}

type shellParams struct {
	Command string `json:"command"`
}

// New returns the shell command tool, backed by fsSys.
func New(fsSys *guestsys.FS, cfg Config) tool.Tool {
	cfg = cfg.withDefaults()

	desc := "Run a shell command in the workspace and return its stdout, stderr, and exit " +
		"code. Commands run through " + cfg.Shell + " with a timeout, so avoid interactive " +
		"programs and long-running servers — they will be killed. Prefer the dedicated " +
		"read_file, glob, and grep tools where they fit; they give structured output and " +
		"cannot be chained into unintended side effects."

	def := tool.ToolDefinition{
		Name:        "shell",
		Description: desc,
		Parameters: tool.Object([]string{"command"}, map[string]tool.Property{
			"command": tool.String("Command line to execute."),
		}),
	}

	return tool.New(def, func(ctx context.Context, p shellParams) (string, error) {
		command := strings.TrimSpace(p.Command)
		if command == "" {
			return "", fmt.Errorf("command must not be empty")
		}

		return fsSys.ExecShell(ctx, guestsys.ExecOptions{
			Command:        command,
			Shell:          cfg.Shell,
			Timeout:        cfg.Timeout,
			MaxOutputBytes: cfg.MaxOutputBytes,
			Env:            cfg.Env,
		})
	})
}
