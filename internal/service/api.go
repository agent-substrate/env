package service

// CreateSandboxRequest is the body of POST /v1/sandboxes.
type CreateSandboxRequest struct {
	// ID is the sandbox identifier (a DNS-1123 label). Required.
	ID string `json:"id"`

	// Template is the name of the ActorTemplate the sandbox is created
	// from. Defaults to the service's default template ("sandbox").
	Template string `json:"template,omitempty"`

	// Namespace is the Kubernetes namespace the ActorTemplate lives in.
	// Defaults to "ate-sandbox", the default namespace of
	// `sbx deploy`.
	Namespace string `json:"namespace,omitempty"`
}

// FSRequest is the body of the filesystem endpoints
// (POST /v1/sandboxes/{id}/{file,dir,stat}).
type FSRequest struct {
	// Path of the file or directory inside the sandbox. Relative paths
	// resolve against the guest's workdir. Required.
	Path string `json:"path"`

	// Mode is the octal file mode for write and mkdir, e.g. "644".
	// Defaults to "644" for files and "755" for directories.
	Mode string `json:"mode,omitempty"`

	// Content is the file content for write. It is base64-encoded in
	// JSON.
	Content []byte `json:"content,omitempty"`
}
