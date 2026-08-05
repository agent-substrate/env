package guest

import (
	"encoding/json"

	"github.com/agent-substrate/sandbox/internal/guest/guestsys"
)

// FunctionCall represents a tool execution request.
type FunctionCall struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Args      json.RawMessage `json:"args"`
	Input     json.RawMessage `json:"input"`
}

// InteractionContent represents a content part of a tool execution result.
type InteractionContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// FunctionResult represents the outcome of a tool execution.
type FunctionResult struct {
	Type    string               `json:"type"`
	Name    string               `json:"name"`
	CallID  string               `json:"call_id"`
	Result  []InteractionContent `json:"result"`
	IsError bool                 `json:"is_error,omitempty"`
}

// CmdRequest describes a command to run inside a sandbox.
type CmdRequest struct {
	// Command is the argv of the process to run. It is executed directly,
	// not through a shell. Use []string{"sh", "-c", "..."} for shell syntax.
	Command []string `json:"command"`

	// Env holds additional environment variables set for the process, on
	// top of the guest daemon's environment.
	Env map[string]string `json:"env,omitempty"`

	// Cwd is the working directory for the process. Defaults to the guest
	// daemon's working directory.
	Cwd string `json:"cwd,omitempty"`

	// Stdin is fed to the process's standard input. It is base64-encoded
	// in JSON.
	Stdin []byte `json:"stdin,omitempty"`

	// Timeout bounds command execution, formatted as a Go duration string
	// (e.g. "30s"). Zero or empty means no timeout beyond the HTTP
	// request's own deadline.
	Timeout string `json:"timeout,omitempty"`
}

// CmdResult is the outcome of a CmdRequest.
type CmdResult struct {
	// Stdout and Stderr hold the captured output, capped at the guest's
	// output limit per stream.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`

	// ExitCode is the process exit code. -1 if the process was killed by
	// a signal or failed to start.
	ExitCode int `json:"exitCode"`

	// TimedOut reports whether the command was killed because it exceeded
	// the requested timeout.
	TimedOut bool `json:"timedOut,omitempty"`

	// StdoutTruncated / StderrTruncated report whether output exceeded the
	// per-stream cap and was cut off.
	StdoutTruncated bool `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool `json:"stderrTruncated,omitempty"`

	// Duration is how long the command ran, as a Go duration string.
	Duration string `json:"duration,omitempty"`
}

// DirEntry describes a file or directory inside the sandbox.
type DirEntry = guestsys.DirEntry

// ListDirResponse is the response of the directory listing endpoint.
type ListDirResponse struct {
	Entries []DirEntry `json:"entries"`
}

// Error codes returned in Error.Code.
const (
	CodeNotFound        = "not_found"
	CodeInvalidArgument = "invalid_argument"
	CodeNotFile         = "not_file"
	CodeNotDirectory    = "not_directory"
	CodeInternal        = "internal"
)

// Error is the JSON error envelope returned by the guest daemon and the
// sbx-api service on non-2xx responses.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return e.Message }
