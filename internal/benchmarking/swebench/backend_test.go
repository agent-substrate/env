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

package swebench

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	env "github.com/agent-substrate/env/clients/go"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeAPI is an in-process ate-env API: enough of the environment, process
// and file services for the backend's start, poll and read path.
type fakeAPI struct {
	ateenvv1alpha.UnimplementedEnvironmentServiceServer
	ateenvv1alpha.UnimplementedProcessServiceServer
	ateenvv1alpha.UnimplementedFileSystemServiceServer

	mu sync.Mutex
	// pollsUntilExit is how many GetProcess calls a process stays running;
	// unavailablePolls is how many GetProcess calls answer Unavailable first.
	pollsUntilExit   int
	unavailablePolls int
	exitCode         int32
	stdout, stderr   string

	nextID   int
	polls    map[string]int
	exited   map[string]bool
	killed   []string
	created  []string
	deleted  []string
	commands []string
}

func (f *fakeAPI) CreateEnvironment(_ context.Context, req *ateenvv1alpha.CreateEnvironmentRequest) (*ateenvv1alpha.CreateEnvironmentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req.GetId())
	return &ateenvv1alpha.CreateEnvironmentResponse{Environment: &ateenvv1alpha.Environment{Id: req.GetId(), Atespace: req.GetAtespace()}}, nil
}

func (f *fakeAPI) DeleteEnvironment(_ context.Context, req *ateenvv1alpha.DeleteEnvironmentRequest) (*ateenvv1alpha.DeleteEnvironmentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, req.GetId())
	return &ateenvv1alpha.DeleteEnvironmentResponse{}, nil
}

func (f *fakeAPI) StartProcess(_ context.Context, req *ateenvv1alpha.StartProcessRequest) (*ateenvv1alpha.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("p%d", f.nextID)
	f.commands = append(f.commands, strings.Join(req.GetCommand(), " "))
	return &ateenvv1alpha.Process{ProcessId: id, State: ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING}, nil
}

func (f *fakeAPI) GetProcess(_ context.Context, req *ateenvv1alpha.GetProcessRequest) (*ateenvv1alpha.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailablePolls > 0 {
		f.unavailablePolls--
		return nil, status.Error(codes.Unavailable, "router: no healthy upstream")
	}
	f.polls[req.GetProcessId()]++
	p := &ateenvv1alpha.Process{ProcessId: req.GetProcessId(), State: ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING}
	if f.polls[req.GetProcessId()] > f.pollsUntilExit {
		f.exited[req.GetProcessId()] = true
		p.State = ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED
		p.ExitCode = f.exitCode
	}
	return p, nil
}

func (f *fakeAPI) StreamProcessOutput(req *ateenvv1alpha.StreamProcessOutputRequest, stream grpc.ServerStreamingServer[ateenvv1alpha.ProcessOutput]) error {
	f.mu.Lock()
	stdout, stderr, exited, code := f.stdout, f.stderr, f.exited[req.GetProcessId()], f.exitCode
	f.mu.Unlock()
	if stdout != "" {
		if err := stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Stdout{Stdout: []byte(stdout)}}); err != nil {
			return err
		}
	}
	if stderr != "" {
		if err := stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Stderr{Stderr: []byte(stderr)}}); err != nil {
			return err
		}
	}
	if exited {
		return stream.Send(&ateenvv1alpha.ProcessOutput{Output: &ateenvv1alpha.ProcessOutput_Exit{Exit: &ateenvv1alpha.Process{
			ProcessId: req.GetProcessId(), State: ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED, ExitCode: code}}})
	}
	return nil
}

func (f *fakeAPI) SignalProcess(_ context.Context, req *ateenvv1alpha.SignalProcessRequest) (*ateenvv1alpha.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, req.GetProcessId())
	f.exited[req.GetProcessId()] = true
	return &ateenvv1alpha.Process{ProcessId: req.GetProcessId(), State: ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED, ExitCode: 137}, nil
}

// newFakeBackend serves fakeAPI over an in-memory connection and returns an
// EnvBackend on it, the same path the binary uses with a real address.
func newFakeBackend(t *testing.T, api *fakeAPI) *EnvBackend {
	t.Helper()
	api.polls = map[string]int{}
	api.exited = map[string]bool{}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	ateenvv1alpha.RegisterEnvironmentServiceServer(srv, api)
	ateenvv1alpha.RegisterProcessServiceServer(srv, api)
	ateenvv1alpha.RegisterFileSystemServiceServer(srv, api)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := env.NewClient(env.ClientOptions{GRPCConn: conn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(); _ = conn.Close(); srv.Stop() })
	return &EnvBackend{clients: []*env.Client{c}, atespace: "ate-env", baseTemplate: "default-template"}
}

func fastPolls(t *testing.T) {
	t.Helper()
	oldMin, oldMax := pollMin, pollMax
	pollMin, pollMax = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { pollMin, pollMax = oldMin, oldMax })
}

func TestEnvBackendRunCollectsExitAndOutput(t *testing.T) {
	fastPolls(t)
	api := &fakeAPI{pollsUntilExit: 3, exitCode: 4, stdout: "out", stderr: "err"}
	b := newFakeBackend(t, api)
	sb, err := b.Create(context.Background(), "e1", "reg/img@"+goodDigest)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Run(context.Background(), "echo hi", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 4 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("result %+v", res)
	}
	if len(api.killed) != 0 {
		t.Fatalf("a process that exited must not be killed: %v", api.killed)
	}
	if len(api.created) != 1 || api.created[0] != "e1" {
		t.Fatalf("created %v", api.created)
	}
	if !strings.HasSuffix(api.commands[0], "echo hi") {
		t.Fatalf("command %q", api.commands[0])
	}
}

func TestEnvBackendRunToleratesTransientPolls(t *testing.T) {
	fastPolls(t)
	api := &fakeAPI{pollsUntilExit: 1, unavailablePolls: pollRetries}
	b := newFakeBackend(t, api)
	sb, _ := b.Create(context.Background(), "e1", "reg/img@"+goodDigest)
	if _, err := sb.Run(context.Background(), "true", time.Minute); err != nil {
		t.Fatalf("%d transient poll errors must be tolerated: %v", pollRetries, err)
	}
}

func TestEnvBackendRunGivesUpAndKills(t *testing.T) {
	fastPolls(t)
	api := &fakeAPI{pollsUntilExit: 100, unavailablePolls: pollRetries + 1}
	b := newFakeBackend(t, api)
	sb, _ := b.Create(context.Background(), "e1", "reg/img@"+goodDigest)
	_, err := sb.Run(context.Background(), "sleep 100", time.Minute)
	if err == nil || status.Code(err) != codes.Unavailable {
		t.Fatalf("expected the poll error after the retry limit, got %v", err)
	}
	if len(api.killed) != 1 {
		t.Fatalf("an abandoned process must be killed once, got %v", api.killed)
	}
}

func TestEnvBackendRunKillsOnCancel(t *testing.T) {
	fastPolls(t)
	api := &fakeAPI{pollsUntilExit: 1 << 30}
	b := newFakeBackend(t, api)
	sb, _ := b.Create(context.Background(), "e1", "reg/img@"+goodDigest)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := sb.Run(ctx, "sleep 100", time.Minute); err == nil {
		t.Fatal("a cancelled Run must return an error")
	}
	if len(api.killed) != 1 {
		t.Fatalf("the process must be killed after a cancel, got %v", api.killed)
	}
}

func TestEnvBackendDeleteByName(t *testing.T) {
	api := &fakeAPI{}
	b := newFakeBackend(t, api)
	if err := b.Delete(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != "gone" {
		t.Fatalf("deleted %v", api.deleted)
	}
}

func TestEnvBackendSpreadsConnections(t *testing.T) {
	// grpc.NewClient does not dial until the first call, so a backend over
	// an unreachable address is enough to check the pool and the rotation.
	b, err := NewEnvBackend("127.0.0.1:1", "ate-env", "default-template", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(b.clients) != 3 {
		t.Fatalf("%d clients, want 3", len(b.clients))
	}
	seen := map[int]int{}
	for i := 0; i < 9; i++ {
		c := b.pick()
		for j, cc := range b.clients {
			if cc == c {
				seen[j]++
			}
		}
	}
	for j := 0; j < 3; j++ {
		if seen[j] != 3 {
			t.Fatalf("connection %d picked %d times, want 3: %v", j, seen[j], seen)
		}
	}
	one, err := NewEnvBackend("127.0.0.1:1", "ate-env", "default-template", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	if len(one.clients) != 1 {
		t.Fatalf("connections 0 must mean 1, got %d", len(one.clients))
	}
}
