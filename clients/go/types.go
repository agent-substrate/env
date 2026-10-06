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

package env

import "time"

// chunkSize is the maximum payload per streamed message.
const chunkSize = 64 * 1024

// ShellRequest describes a shell command line to run inside an env.
type ShellRequest struct {
	// Command is the shell command line to run inside the environment.
	Command string `json:"command"`

	// Env holds additional environment variables set for the process, on
	// top of the guest daemon's environment.
	Env map[string]string `json:"env,omitempty"`

	// Cwd is the working directory for the process. Defaults to the guest
	// daemon's working directory.
	Cwd string `json:"cwd,omitempty"`

	// Stdin is fed to the process's standard input, then stdin is closed.
	// Nil leaves stdin empty. It is base64-encoded in JSON.
	Stdin []byte `json:"stdin,omitempty"`

	// Timeout kills the command with SIGKILL after this duration. Zero uses
	// the guest default.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// ShellResponse is the outcome of a ShellRequest.
type ShellResponse struct {
	// Stdout and Stderr hold the captured output.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`

	// ExitCode is the process exit code, or 128 + signal number if the
	// process was killed by a signal (e.g. 137 for SIGKILL).
	ExitCode int `json:"exit_code"`
}
