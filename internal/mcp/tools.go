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
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultChunkSize = 64 * 1024

// NewFileSystemTools returns MCP tools for FileSystemService RPCs.
func NewFileSystemTools(client ateenvv1.FileSystemServiceClient) []tool.Tool {
	return []tool.Tool{
		readFileTool(client),
		writeFileTool(client),
	}
}

// NewProcessTools returns MCP tools for ProcessService RPCs.
func NewProcessTools(client ateenvv1.ProcessServiceClient) []tool.Tool {
	return []tool.Tool{
		shellTool(client),
		startProcessTool(client),
		getProcessTool(client),
		streamProcessLogsTool(client),
		killProcessTool(client),
	}
}

// NewTools returns all MCP tools backed by FileSystemService and ProcessService clients.
func NewTools(fsClient ateenvv1.FileSystemServiceClient, procClient ateenvv1.ProcessServiceClient) []tool.Tool {
	var tools []tool.Tool
	if fsClient != nil {
		tools = append(tools, NewFileSystemTools(fsClient)...)
	}
	if procClient != nil {
		tools = append(tools, NewProcessTools(procClient)...)
	}
	return tools
}

// New returns all MCP tools backed by FileSystemService and ProcessService clients.
func New(fsClient ateenvv1.FileSystemServiceClient, procClient ateenvv1.ProcessServiceClient) []tool.Tool {
	return NewTools(fsClient, procClient)
}

// --- read_file ---------------------------------------------------------------

type readFileParams struct {
	Path string `json:"path"`
}

func readFileTool(client ateenvv1.FileSystemServiceClient) tool.Tool {
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
		stream, err := client.ReadFile(ctx, &ateenvv1.ReadFileRequest{Path: p.Path})
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

func writeFileTool(client ateenvv1.FileSystemServiceClient) tool.Tool {
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
			if err := stream.Send(&ateenvv1.WriteFileRequest{
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
				req := &ateenvv1.WriteFileRequest{
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

func shellTool(client ateenvv1.ProcessServiceClient) tool.Tool {
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

func runProcessToCompletion(ctx context.Context, client ateenvv1.ProcessServiceClient, command []string, cwd string, env map[string]string) (string, error) {
	startResp, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
		Command: command,
		Cwd:     cwd,
		Env:     env,
	})
	if err != nil {
		return "", fmt.Errorf("process start failed: %w", err)
	}
	procID := startResp.GetProcessId()

	stream, err := client.StreamProcessLogs(ctx, &ateenvv1.StreamProcessLogsRequest{
		ProcessId: procID,
		Follow:    true,
	})
	if err != nil {
		return "", fmt.Errorf("stream logs failed: %w", err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading process log chunk: %w", err)
		}
		switch chunk.GetSource() {
		case ateenvv1.LogSource_LOG_SOURCE_STDOUT:
			stdoutBuf.Write(chunk.GetData())
		case ateenvv1.LogSource_LOG_SOURCE_STDERR:
			stderrBuf.Write(chunk.GetData())
		}
	}

	proc, err := client.GetProcess(ctx, &ateenvv1.GetProcessRequest{ProcessId: procID})
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

// --- start_process -----------------------------------------------------------

type startProcessParams struct {
	Command []string          `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func startProcessTool(client ateenvv1.ProcessServiceClient) tool.Tool {
	def := &mcp.Tool{
		Name:        "start_process",
		Description: "Start an asynchronous process in the background via ProcessService gRPC API.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Command binary and arguments array (e.g. [\"ls\", \"-la\"]).",
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
	return tool.New(def, func(ctx context.Context, p startProcessParams) (string, error) {
		if len(p.Command) == 0 {
			return "", errors.New("command must not be empty")
		}
		resp, err := client.StartProcess(ctx, &ateenvv1.StartProcessRequest{
			Command: p.Command,
			Cwd:     p.Cwd,
			Env:     p.Env,
		})
		if err != nil {
			return "", fmt.Errorf("start_process failed: %w", err)
		}
		return fmt.Sprintf("Process started with ID: %s", resp.GetProcessId()), nil
	})
}

// --- get_process -------------------------------------------------------------

type getProcessParams struct {
	ProcessID string `json:"process_id"`
}

func getProcessTool(client ateenvv1.ProcessServiceClient) tool.Tool {
	def := &mcp.Tool{
		Name:        "get_process",
		Description: "Get metadata, status, exit code, and timestamps of a process via ProcessService gRPC API.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"process_id": map[string]any{"type": "string", "description": "Unique process identifier."},
			},
			"required": []string{"process_id"},
		},
	}
	return tool.New(def, func(ctx context.Context, p getProcessParams) (string, error) {
		if strings.TrimSpace(p.ProcessID) == "" {
			return "", errors.New("process_id must not be empty")
		}
		proc, err := client.GetProcess(ctx, &ateenvv1.GetProcessRequest{ProcessId: p.ProcessID})
		if err != nil {
			return "", fmt.Errorf("get_process failed: %w", err)
		}

		var startedStr, finishedStr string
		if proc.GetStartedAt() != nil {
			startedStr = proc.GetStartedAt().AsTime().Format("2006-01-02T15:04:05Z07:00")
		}
		if proc.GetFinishedAt() != nil {
			finishedStr = proc.GetFinishedAt().AsTime().Format("2006-01-02T15:04:05Z07:00")
		}

		return fmt.Sprintf("Process ID: %s\nStatus: %s\nExit Code: %d\nStarted At: %s\nFinished At: %s",
			proc.GetProcessId(), proc.GetStatus().String(), proc.GetExitCode(), startedStr, finishedStr), nil
	})
}

// --- stream_process_logs -----------------------------------------------------

type streamProcessLogsParams struct {
	ProcessID    string `json:"process_id"`
	StdoutOffset int64  `json:"stdout_offset,omitempty"`
	StderrOffset int64  `json:"stderr_offset,omitempty"`
	Follow       bool   `json:"follow,omitempty"`
}

func streamProcessLogsTool(client ateenvv1.ProcessServiceClient) tool.Tool {
	def := &mcp.Tool{
		Name:        "stream_process_logs",
		Description: "Retrieve stdout and stderr logs of a process via ProcessService gRPC API.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"process_id":    map[string]any{"type": "string", "description": "Unique process identifier."},
				"stdout_offset": map[string]any{"type": "integer", "description": "Byte offset for stdout."},
				"stderr_offset": map[string]any{"type": "integer", "description": "Byte offset for stderr."},
				"follow":        map[string]any{"type": "boolean", "description": "Follow live logs until process finishes."},
			},
			"required": []string{"process_id"},
		},
	}
	return tool.New(def, func(ctx context.Context, p streamProcessLogsParams) (string, error) {
		if strings.TrimSpace(p.ProcessID) == "" {
			return "", errors.New("process_id must not be empty")
		}
		stream, err := client.StreamProcessLogs(ctx, &ateenvv1.StreamProcessLogsRequest{
			ProcessId:    p.ProcessID,
			StdoutOffset: p.StdoutOffset,
			StderrOffset: p.StderrOffset,
			Follow:       p.Follow,
		})
		if err != nil {
			return "", fmt.Errorf("stream_process_logs failed: %w", err)
		}

		var stdoutBuf, stderrBuf bytes.Buffer
		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", fmt.Errorf("reading process log chunk: %w", err)
			}
			switch chunk.GetSource() {
			case ateenvv1.LogSource_LOG_SOURCE_STDOUT:
				stdoutBuf.Write(chunk.GetData())
			case ateenvv1.LogSource_LOG_SOURCE_STDERR:
				stderrBuf.Write(chunk.GetData())
			}
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
		if out.Len() == 0 {
			return "(no output)", nil
		}
		return out.String(), nil
	})
}

// --- kill_process ------------------------------------------------------------

type killProcessParams struct {
	ProcessID string `json:"process_id"`
}

func killProcessTool(client ateenvv1.ProcessServiceClient) tool.Tool {
	def := &mcp.Tool{
		Name:        "kill_process",
		Description: "Terminate a running background process via ProcessService gRPC API.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"process_id": map[string]any{"type": "string", "description": "Unique process identifier."},
			},
			"required": []string{"process_id"},
		},
	}
	return tool.New(def, func(ctx context.Context, p killProcessParams) (string, error) {
		if strings.TrimSpace(p.ProcessID) == "" {
			return "", errors.New("process_id must not be empty")
		}
		resp, err := client.KillProcess(ctx, &ateenvv1.KillProcessRequest{ProcessId: p.ProcessID})
		if err != nil {
			return "", fmt.Errorf("kill_process failed: %w", err)
		}
		return fmt.Sprintf("Process %s terminated with exit code %d", p.ProcessID, resp.GetExitCode()), nil
	})
}
