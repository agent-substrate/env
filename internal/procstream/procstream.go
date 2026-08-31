// Package procstream collects process output over ProcessService streams
// that are treated as disposable. The atenet router bounds every request
// (its route timeout defaults to 10s) and an environment can be suspended
// and resumed mid-command, so a single StreamProcessOutputs call must not
// be assumed to live for the whole process. The durable handles are the
// process id and the per-stream byte offsets, never a connection: whenever
// the stream breaks while the process is still running, Collect reopens it
// from the last offsets and continues, invisibly to the caller.
package procstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
)

// maxConsecutiveFailures bounds reconnection attempts that make no progress
// (no bytes received and no process status obtained) before giving up.
const maxConsecutiveFailures = 8

// reopenDelay is the pause before reopening a broken stream.
const reopenDelay = 500 * time.Millisecond

// Options configures Collect.
type Options struct {
	// StdoutOffset and StderrOffset are the byte offsets to start reading from.
	StdoutOffset int64
	StderrOffset int64
	// Follow keeps collecting until the process exits and its output is
	// drained. Without it, Collect returns the output available now.
	Follow bool
}

// Collect gathers stdout and stderr of process id via client, reopening the
// underlying stream from the last byte offsets whenever it breaks while the
// process still runs.
func Collect(ctx context.Context, client ateenvv1.ProcessServiceClient, id string, opts Options) (stdout, stderr []byte, err error) {
	stdoutOffset, stderrOffset := opts.StdoutOffset, opts.StderrOffset
	failures := 0

	for {
		stream, err := client.StreamProcessOutputs(ctx, &ateenvv1.StreamProcessOutputsRequest{
			ProcessId:    id,
			StdoutOffset: stdoutOffset,
			StderrOffset: stderrOffset,
			Follow:       opts.Follow,
		})
		if err == nil {
			for {
				chunk, rerr := stream.Recv()
				if rerr != nil {
					err = rerr
					break
				}
				failures = 0
				switch chunk.GetSource() {
				case ateenvv1.OutputSource_OUTPUT_SOURCE_STDOUT:
					stdout = append(stdout, chunk.GetData()...)
					stdoutOffset += int64(len(chunk.GetData()))
				case ateenvv1.OutputSource_OUTPUT_SOURCE_STDERR:
					stderr = append(stderr, chunk.GetData()...)
					stderrOffset += int64(len(chunk.GetData()))
				}
			}
		}
		if errors.Is(err, io.EOF) {
			// The server completed the stream: in follow mode the process has
			// exited and its output is drained; otherwise the snapshot is done.
			return stdout, stderr, nil
		}
		if ctx.Err() != nil {
			return stdout, stderr, ctx.Err()
		}

		// The stream broke. If the process is finished, one more non-follow
		// pass drains what the broken stream missed; if it still runs, reopen
		// from the current offsets.
		proc, gerr := client.GetProcess(ctx, &ateenvv1.GetProcessRequest{ProcessId: id})
		if gerr == nil && proc.GetStatus() != ateenvv1.ProcessStatus_PROCESS_STATUS_RUNNING && opts.Follow {
			opts.Follow = false
			failures = 0
		} else {
			failures++
		}
		if failures >= maxConsecutiveFailures {
			return stdout, stderr, fmt.Errorf("collecting output of process %s: %w", id, err)
		}

		select {
		case <-ctx.Done():
			return stdout, stderr, ctx.Err()
		case <-time.After(reopenDelay):
		}
	}
}
