package env

import (
	"time"
)

// ShellRequest describes a command to run inside an env.
type ShellRequest struct {
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
}

// ShellResponse is the outcome of a ShellRequest.
type ShellResponse struct {
	// Stdout and Stderr hold the captured output, capped at the guest's
	// output limit per stream.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`

	// ExitCode is the process exit code. -1 if the process was killed by
	// a signal or failed to start.
	ExitCode int `json:"exitCode"`

	// StdoutTruncated / StderrTruncated report whether output exceeded the
	// per-stream cap and was cut off.
	StdoutTruncated bool `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool `json:"stderrTruncated,omitempty"`
}

// DirEntry describes a file or directory inside an environment.
type DirEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path,omitempty"`
	Size       int64     `json:"size,omitempty"`
	Mode       uint32    `json:"mode,omitempty"`
	ModeString string    `json:"modeString,omitempty"`
	ModTime    time.Time `json:"modTime,omitempty"`
	IsDir      bool      `json:"isDir,omitempty"`
}

// CreateEnvRequest is the body of POST /v1/envs.
type CreateEnvRequest struct {
	// ID is the environment identifier (a DNS-1123 label). Required.
	ID string `json:"id"`

	// Template is the name of the ActorTemplate the environment is created
	// from. Defaults to the service's default template.
	Template string `json:"template,omitempty"`

	// Namespace is the Kubernetes namespace the ActorTemplate lives in.
	// Defaults to "ate-env", the default namespace of
	// `ate-env deploy`.
	Namespace string `json:"namespace,omitempty"`
}

// ReadFileRequest is the request body for reading a file.
type ReadFileRequest struct {
	// Path of the file inside the environment. Required.
	Path string `json:"path"`
}

// ReadFileResponse is the response of the read file endpoint.
type ReadFileResponse struct {
	Content []byte `json:"content"`
	Mode    string `json:"mode,omitempty"`
	Size    int64  `json:"size"`
}

// WriteFileRequest is the request body for writing a file.
type WriteFileRequest struct {
	// Path of the file inside the environment. Required.
	Path string `json:"path"`

	// Mode is the octal file mode, e.g. "644".
	Mode string `json:"mode,omitempty"`

	// Content is the file content.
	Content []byte `json:"content,omitempty"`
}

// ListDirRequest is the request body for directory listing.
type ListDirRequest struct {
	// Path of the directory inside the environment. Required.
	Path string `json:"path"`
}

// ListDirResponse is the response of the directory listing endpoint.
type ListDirResponse struct {
	Entries []DirEntry `json:"entries"`
}

// MkdirRequest is the request body for directory creation.
type MkdirRequest struct {
	// Path of the directory inside the environment. Required.
	Path string `json:"path"`

	// Mode is the octal directory mode, e.g. "755".
	Mode string `json:"mode,omitempty"`
}

// StatRequest is the request body for getting file or directory info.
type StatRequest struct {
	// Path of the file or directory inside the environment. Required.
	Path string `json:"path"`
}

// RemoveRequest is the request body for deleting a file or directory.
type RemoveRequest struct {
	// Path of the file or directory inside the environment. Required.
	Path string `json:"path"`
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
// ate-env-api service on non-2xx responses.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return e.Message }
