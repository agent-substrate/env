package env

// ShellRequest describes a command to run inside an env.
type ShellRequest struct {
	// Command is the shell command line to run inside the environment.
	Command string `json:"command"`

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
	// Stdout and Stderr hold the captured output.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`

	// ExitCode is the process exit code. -1 if the process was killed by
	// a signal or failed to start.
	ExitCode int `json:"exit_code"`
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

// EnvInfo describes an environment as reported by the API service.
type EnvInfo struct {
	// ID is the environment's identifier.
	ID string `json:"id"`

	// Atespace is the Substrate atespace the environment lives in.
	Atespace string `json:"atespace"`

	// Template and TemplateNamespace identify the ActorTemplate the
	// environment was created from.
	Template          string `json:"template,omitempty"`
	TemplateNamespace string `json:"template_namespace,omitempty"`

	// Status is the environment's lifecycle status, e.g. "running" or
	// "suspended".
	Status string `json:"status"`
}

// Error codes returned in Error.Code.
const (
	CodeNotFound        = "not_found"
	CodeInvalidArgument = "invalid_argument"
	CodeNotFile            = "not_file"
	CodeNotDirectory       = "not_directory"
	CodeInternal           = "internal"
)

// Error is the JSON error envelope returned by the guest daemon and the
// ate-env-api service on non-2xx responses.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return e.Message }
