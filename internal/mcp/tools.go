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

// Package mcp provides MCP (Model Context Protocol) tool adapters backed by
// FileSystemService and ProcessService gRPC clients, served by ate-env-api.
package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/agent-substrate/env/internal/tool"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultChunkSize = 64 * 1024

// NewFileSystemTools returns MCP tools for FileSystemService RPCs.
func NewFileSystemTools(client ateenvv1alpha.FileSystemServiceClient) []tool.Tool {
	return []tool.Tool{
		readFileTool(client),
		writeFileTool(client),
	}
}

// NewProcessTools returns MCP tools for ProcessService RPCs.
func NewProcessTools(client ateenvv1alpha.ProcessServiceClient) []tool.Tool {
	return []tool.Tool{
		shellTool(client),
	}
}

// NewTools returns all MCP tools backed by FileSystemService and ProcessService clients.
func NewTools(fsClient ateenvv1alpha.FileSystemServiceClient, procClient ateenvv1alpha.ProcessServiceClient) []tool.Tool {
	var tools []tool.Tool
	if fsClient != nil {
		tools = append(tools, NewFileSystemTools(fsClient)...)
	}
	if procClient != nil {
		tools = append(tools, NewProcessTools(procClient)...)
	}
	return tools
}

// --- read_file ---------------------------------------------------------------

type readFileParams struct {
	Path string `json:"path"`
}

func readFileTool(client ateenvv1alpha.FileSystemServiceClient) tool.Tool {
	def := &mcp.Tool{
		Name:        "read_file",
		Description: "Read a file from the environment filesystem via FileSystemService gRPC API.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Absolute or workspace-relative file path."},
			},
			"required": []string{"path"},
		},
	}
	return tool.New(def, func(ctx context.Context, p readFileParams) (string, error) {
		if strings.TrimSpace(p.Path) == "" {
			return "", errors.New("path must not be empty")
		}
		stream, err := client.ReadFile(ctx, &ateenvv1alpha.ReadFileRequest{Path: p.Path})
		if err != nil {
			return "", fmt.Errorf("read_file failed: %w", err)
		}

		var buf bytes.Buffer
		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", fmt.Errorf("reading file chunk: %w", err)
			}
			buf.Write(chunk.GetData())
		}
		return buf.String(), nil
	})
}

// --- write_file --------------------------------------------------------------

type writeFileParams struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
}

func writeFileTool(client ateenvv1alpha.FileSystemServiceClient) tool.Tool {
	def := &mcp.Tool{
		Name:        "write_file",
		Description: "Write content to a file in the environment filesystem via FileSystemService gRPC API.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Absolute or workspace-relative file path."},
				"content": map[string]any{"type": "string", "description": "Text or binary data content to write."},
				"mode":    map[string]any{"type": "integer", "description": "Optional POSIX file permission mode (e.g. 0644, 0755)."},
			},
			"required": []string{"path", "content"},
		},
	}
	return tool.New(def, func(ctx context.Context, p writeFileParams) (string, error) {
		if strings.TrimSpace(p.Path) == "" {
			return "", errors.New("path must not be empty")
		}
		stream, err := client.WriteFile(ctx)
		if err != nil {
			return "", fmt.Errorf("write_file client error: %w", err)
		}

		data := []byte(p.Content)
		if len(data) == 0 {
			if err := stream.Send(&ateenvv1alpha.WriteFileRequest{
				Path: p.Path,
				Mode: p.Mode,
			}); err != nil {
				return "", fmt.Errorf("sending write request header: %w", err)
			}
		} else {
			for i := 0; i < len(data); i += defaultChunkSize {
				end := i + defaultChunkSize
				if end > len(data) {
					end = len(data)
				}
				req := &ateenvv1alpha.WriteFileRequest{
					Chunk: data[i:end],
				}
				if i == 0 {
					req.Path = p.Path
					req.Mode = p.Mode
				}
				if err := stream.Send(req); err != nil {
					return "", fmt.Errorf("sending write chunk: %w", err)
				}
			}
		}

		resp, err := stream.CloseAndRecv()
		if err != nil {
			return "", fmt.Errorf("completing write_file stream: %w", err)
		}
		return fmt.Sprintf("Wrote %d bytes to %s", resp.GetBytesWritten(), p.Path), nil
	})
}

// --- shell -------------------------------------------------------------------

type shellParams struct {
	Command string            `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func shellTool(client ateenvv1alpha.ProcessServiceClient) tool.Tool {
	def := &mcp.Tool{
		Name:        "shell",
		Description: "Run a shell command line inside the environment using ProcessService gRPC API and return output and exit code.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command line to execute (e.g. \"ls -la\").",
				},
				"cwd": map[string]any{"type": "string", "description": "Optional working directory."},
				"env": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Optional environment variables.",
				},
			},
			"required": []string{"command"},
		},
	}
	return tool.New(def, func(ctx context.Context, p shellParams) (string, error) {
		command := strings.TrimSpace(p.Command)
		if command == "" {
			return "", errors.New("command must not be empty")
		}
		return runProcessToCompletion(ctx, client, []string{"sh", "-c", command}, p.Cwd, p.Env)
	})
}

func runProcessToCompletion(ctx context.Context, client ateenvv1alpha.ProcessServiceClient, command []string, cwd string, env map[string]string) (string, error) {
	startResp, err := client.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{
		Command: command,
		Cwd:     cwd,
		Env:     env,
	})
	if err != nil {
		return "", fmt.Errorf("process start failed: %w", err)
	}
	procID := startResp.GetProcessId()

	stream, err := client.StreamProcessOutputs(ctx, &ateenvv1alpha.StreamProcessOutputsRequest{
		ProcessId: procID,
		Follow:    true,
	})
	if err != nil {
		return "", fmt.Errorf("stream outputs failed: %w", err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading process output chunk: %w", err)
		}
		switch chunk.GetSource() {
		case ateenvv1alpha.OutputSource_OUTPUT_SOURCE_STDOUT:
			stdoutBuf.Write(chunk.GetData())
		case ateenvv1alpha.OutputSource_OUTPUT_SOURCE_STDERR:
			stderrBuf.Write(chunk.GetData())
		}
	}

	proc, err := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: procID})
	if err != nil {
		return "", fmt.Errorf("get process status failed: %w", err)
	}

	var out strings.Builder
	if stdoutBuf.Len() > 0 {
		out.WriteString(stdoutBuf.String())
	}
	if stderrBuf.Len() > 0 {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString("\n")
		}
		out.WriteString("[STDERR]\n" + stderrBuf.String())
	}
	if proc.GetExitCode() != 0 {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString("\n")
		}
		out.WriteString(fmt.Sprintf("[Exit Code: %d]", proc.GetExitCode()))
	}
	if out.Len() == 0 {
		return "(no output)", nil
	}
	return out.String(), nil
}
