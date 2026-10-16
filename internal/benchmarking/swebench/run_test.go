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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeBackend is an in-memory Backend that records calls and can fail on
// demand. Its sandboxes answer every command with exit code 0 unless the
// command names a nonzero exit (e.g. "exit 1").
type fakeBackend struct {
	mu          sync.Mutex
	created     map[string]int
	suspended   map[string]int
	deleted     map[string]int
	commands    []string
	files       map[string]string
	failCreate  map[string]bool
	unavailable int32 // first-command Unavailable errors still to return

	inflight    atomic.Int32
	maxInflight atomic.Int32
}

func newFake() *fakeBackend {
	return &fakeBackend{created: map[string]int{}, suspended: map[string]int{}, deleted: map[string]int{},
		files: map[string]string{}, failCreate: map[string]bool{}}
}

func (f *fakeBackend) Create(ctx context.Context, id, image string) (Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created[id]++
	if f.failCreate[id] {
		return nil, errors.New("create refused")
	}
	return &fakeSandbox{b: f, id: id}, nil
}

type fakeSandbox struct {
	b  *fakeBackend
	id string
}

func (s *fakeSandbox) enter() func() {
	n := s.b.inflight.Add(1)
	for {
		m := s.b.maxInflight.Load()
		if n <= m || s.b.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	return func() { s.b.inflight.Add(-1) }
}

func (s *fakeSandbox) Run(ctx context.Context, command string, timeout time.Duration) (Result, error) {
	defer s.enter()()
	if atomic.AddInt32(&s.b.unavailable, -1) >= 0 {
		return Result{}, status.Error(codes.Unavailable, "waking")
	}
	atomic.AddInt32(&s.b.unavailable, 1)
	time.Sleep(time.Millisecond)
	s.b.mu.Lock()
	s.b.commands = append(s.b.commands, s.id+": "+command)
	s.b.mu.Unlock()
	if strings.HasPrefix(command, "exit ") {
		return Result{ExitCode: int(command[5] - '0'), Stderr: "failed"}, nil
	}
	return Result{Stdout: "ok"}, nil
}

func (s *fakeSandbox) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error {
	defer s.enter()()
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.b.files[s.id+":"+path] = string(content)
	return nil
}

func (s *fakeSandbox) Suspend(ctx context.Context) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.b.suspended[s.id]++
	return nil
}

func (s *fakeSandbox) Delete(ctx context.Context) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.b.deleted[s.id]++
	return nil
}

func instances(n int) []Instance {
	out := make([]Instance, n)
	for i := range out {
		out[i] = Instance{InstanceID: "repo__repo-" + string(rune('a'+i)), Image: "reg/img@" + goodDigest,
			Patch: "PATCH", EvalScript: "EVAL"}
	}
	return out
}

func testTrajectory(t *testing.T, rewardCmd string) *Trajectory {
	t.Helper()
	tr, err := ParseTrajectory([]byte(`
name: test
setup:
  - {name: baseline, run: "git commit baseline", expect_exit: 0}
reset:
  - {name: reset, run: "git checkout", expect_exit: 0}
steps:
  - {name: write, write: {path: /tmp/p, content: "{{.Patch}}"}}
  - {name: apply, run: "git apply", expect_exit: 0}
  - {name: tests, run: "` + rewardCmd + `", reward: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func baseConfig(t *testing.T, b Backend, out *bytes.Buffer) Config {
	t.Helper()
	var w io.Writer
	if out != nil {
		w = out
	}
	return Config{
		RunID:       "t",
		Backend:     b,
		Instances:   instances(3),
		Trajectory:  testTrajectory(t, "true"),
		Recorder:    NewRecorder(w),
		Logger:      slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Width:       2,
		WakeBackoff: time.Millisecond,
	}
}

func TestEvalCreatesReplaysDeletes(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	cfg := baseConfig(t, f, &out)
	cfg.Mode = ModeEval
	cfg.TasksPerImage = 2

	sum, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Tasks != 6 || sum.Failed != 0 || sum.Rewarded != 6 {
		t.Fatalf("summary %+v", sum)
	}
	if len(f.created) != 6 || len(f.deleted) != 6 {
		t.Fatalf("created %d deleted %d", len(f.created), len(f.deleted))
	}
	for id, n := range f.created {
		if n != 1 || f.deleted[id] != 1 {
			t.Fatalf("env %s created %d deleted %d", id, n, f.deleted[id])
		}
	}
	if got := f.maxInflight.Load(); got > 2 {
		t.Fatalf("max in flight %d > width 2", got)
	}
	if f.files["t-repo--repo-a-0:/tmp/p"] != "PATCH" {
		t.Fatalf("patch not written: %v", f.files)
	}
	resets, setups := countCommands(f, "git checkout", "git commit baseline")
	if resets != 0 || setups != 6 {
		t.Fatalf("eval mode: %d resets (want 0), %d setups (want 6, once per environment)", resets, setups)
	}
	rep := Build(parseEvents(t, out.String()))
	if rep.Tasks != 6 || rep.RewardRate != 1 {
		t.Fatalf("report %+v", rep)
	}
	for _, ev := range []string{EventCreate, EventTTFI, EventTask, EventDelete, EventRun} {
		if !hasStat(rep, ev) {
			t.Errorf("no %s events", ev)
		}
	}
}

func TestEvalCountsFailures(t *testing.T) {
	f := newFake()
	f.failCreate[EnvID("t", "repo__repo-a", 0)] = true
	var out bytes.Buffer
	cfg := baseConfig(t, f, &out)
	cfg.Mode = ModeEval
	cfg.TasksPerImage = 1
	cfg.Trajectory = testTrajectory(t, "exit 1")

	sum, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// One create failure; the other two run to the reward step and get 0.
	if sum.Tasks != 3 || sum.Failed != 1 || sum.Rewarded != 0 {
		t.Fatalf("summary %+v", sum)
	}
	rep := Build(parseEvents(t, out.String()))
	if rep.RewardRate != 0 || rep.Failed != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestExpectExitFailsTask(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	cfg := baseConfig(t, f, &out)
	cfg.Mode = ModeEval
	cfg.TasksPerImage = 1
	tr, err := ParseTrajectory([]byte("name: t\nsteps:\n  - {name: a, run: \"exit 2\", expect_exit: 0}\n  - {name: b, run: \"true\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Trajectory = tr
	sum, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 3 {
		t.Fatalf("summary %+v", sum)
	}
	for _, c := range f.commands {
		if strings.HasSuffix(c, ": true") {
			t.Fatal("steps after a failed expectation must not run")
		}
	}
}

func TestStepsSuspendPark(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	cfg := baseConfig(t, f, &out)
	cfg.Mode = ModeSteps
	cfg.Rollouts = 2
	cfg.Steps = 3
	cfg.Park = ParkSuspend

	sum, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Tasks != 18 || sum.Failed != 0 || sum.Rewarded != 18 {
		t.Fatalf("summary %+v", sum)
	}
	if len(f.created) != 6 {
		t.Fatalf("created %d envs, want 6 created once", len(f.created))
	}
	for id, n := range f.created {
		if n != 1 || f.suspended[id] != 2 || f.deleted[id] != 1 {
			t.Fatalf("env %s created %d suspended %d deleted %d", id, n, f.suspended[id], f.deleted[id])
		}
	}
	resets, setups := countCommands(f, "git checkout", "git commit baseline")
	if resets != 12 || setups != 6 {
		t.Fatalf("suspend park: %d resets (want 6 envs x 2 later steps), %d setups (want 6, once per environment)", resets, setups)
	}
	rep := Build(parseEvents(t, out.String()))
	if len(rep.Steps) != 3 || rep.Steps[0].Tasks != 6 || rep.Steps[0].ParkMS == 0 || rep.Steps[2].ParkMS != 0 {
		t.Fatalf("steps %+v", rep.Steps)
	}
	if s := stat(rep, EventWake); s.Count != 12 {
		t.Fatalf("wake events %+v, want 12 (6 envs x steps 2 and 3)", s)
	}
	if s := stat(rep, EventTTFI); s.Count != 6 {
		t.Fatalf("ttfi events %+v, want 6", s)
	}
}

func TestStepsDeleteParkRecreates(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	cfg := baseConfig(t, f, &out)
	cfg.Mode = ModeSteps
	cfg.Rollouts = 1
	cfg.Steps = 2
	cfg.Park = ParkDelete

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// Each step creates under a fresh name: 3 environments x 2 steps.
	if len(f.created) != 6 {
		t.Fatalf("created %d distinct names, want 6: %v", len(f.created), f.created)
	}
	fresh := 0
	for id, n := range f.created {
		if n != 1 || f.deleted[id] != 1 || f.suspended[id] != 0 {
			t.Fatalf("env %s created %d deleted %d suspended %d", id, n, f.deleted[id], f.suspended[id])
		}
		if strings.HasSuffix(id, "-s2") {
			fresh++
		}
	}
	if fresh != 3 {
		t.Fatalf("%d step-2 names, want 3", fresh)
	}
	rep := Build(parseEvents(t, out.String()))
	if s := stat(rep, EventTTFI); s.Count != 6 {
		t.Fatalf("ttfi events %+v, want 3 envs x 2 creates", s)
	}
	if hasStat(rep, EventWake) {
		t.Fatal("delete park has no wake events")
	}
	resets, setups := countCommands(f, "git checkout", "git commit baseline")
	if resets != 0 || setups != 6 {
		t.Fatalf("delete park: %d resets (want 0, every environment is new), %d setups (want 3 envs x 2 creates)", resets, setups)
	}
}

// countCommands counts the fake's recorded commands containing each needle.
func countCommands(f *fakeBackend, resetNeedle, setupNeedle string) (resets, setups int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.Contains(c, resetNeedle) {
			resets++
		}
		if strings.Contains(c, setupNeedle) {
			setups++
		}
	}
	return resets, setups
}

func TestStepsKeepLeavesFleet(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	cfg := baseConfig(t, f, &out)
	cfg.Mode = ModeSteps
	cfg.Rollouts = 1
	cfg.Steps = 1
	cfg.Park = ParkNone
	cfg.Keep = true
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 0 || len(f.suspended) != 0 {
		t.Fatalf("keep: deleted %v suspended %v", f.deleted, f.suspended)
	}
}

func TestFirstCommandRetriesUnavailable(t *testing.T) {
	f := newFake()
	f.unavailable = 2
	var out bytes.Buffer
	cfg := baseConfig(t, f, &out)
	cfg.Mode = ModeEval
	cfg.TasksPerImage = 1
	cfg.Instances = instances(1)
	cfg.WakeBudget = time.Second
	tr, err := ParseTrajectory([]byte("name: t\nsteps:\n  - {name: a, run: \"true\", reward: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Trajectory = tr
	sum, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 0 {
		t.Fatalf("summary %+v", sum)
	}

	rep := Build(parseEvents(t, out.String()))
	if s := stat(rep, EventTTFI); s.Count != 1 {
		t.Fatalf("ttfi %+v", s)
	}
	for _, e := range parseEvents(t, out.String()) {
		if e.Event == EventTTFI && e.Attempts != 3 {
			t.Fatalf("ttfi attempts = %d, want 3", e.Attempts)
		}
	}

	f = newFake()
	f.unavailable = 1000
	cfg.Backend = f
	cfg.WakeBudget = 5 * time.Millisecond
	cfg.WakeBackoff = 2 * time.Millisecond
	sum, err = Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 1 {
		t.Fatalf("budget exhausted must fail the task: %+v", sum)
	}
}

func TestIsTransient(t *testing.T) {
	for code, want := range map[codes.Code]bool{
		codes.Unavailable: true, codes.Unknown: true, codes.Internal: true, codes.Aborted: true, codes.DeadlineExceeded: true,
		codes.InvalidArgument: false, codes.NotFound: false, codes.FailedPrecondition: false, codes.AlreadyExists: false,
	} {
		if got := isTransient(status.Error(code, "x")); got != want {
			t.Errorf("%v: transient = %v, want %v", code, got, want)
		}
	}
	if isTransient(errors.New("plain")) {
		t.Error("a non-status error is not transient")
	}
}

func TestConfigValidation(t *testing.T) {
	f := newFake()
	good := baseConfig(t, f, nil)
	good.Mode = ModeEval
	good.TasksPerImage = 1
	bad := []func(c *Config){
		func(c *Config) { c.RunID = "" },
		func(c *Config) { c.Backend = nil },
		func(c *Config) { c.Instances = nil },
		func(c *Config) { c.Trajectory = nil },
		func(c *Config) { c.Width = 0 },
		func(c *Config) { c.TasksPerImage = 0 },
		func(c *Config) { c.Mode = "bogus" },
		func(c *Config) { c.Mode = ModeSteps; c.Rollouts = 0; c.Steps = 1 },
		func(c *Config) { c.Mode = ModeSteps; c.Rollouts = 1; c.Steps = 1; c.Park = "bogus" },
	}
	for i, mutate := range bad {
		c := good
		mutate(&c)
		if _, err := Run(context.Background(), c); err == nil {
			t.Errorf("case %d: expected a validation error", i)
		}
	}
}

func TestEnvID(t *testing.T) {
	if got := EnvID("swe-1", "astropy__astropy-7336", 2); got != "swe-1-astropy--astropy-7336-2" {
		t.Fatalf("got %q", got)
	}
	long := EnvID("r", strings.Repeat("x", 80)+"__y", 0)
	if len(long) > 63 {
		t.Fatalf("too long: %q", long)
	}
	if long == EnvID("r", strings.Repeat("x", 80)+"__z", 0) {
		t.Fatal("truncated ids must stay distinct")
	}
}

func parseEvents(t *testing.T, jsonl string) []Event {
	t.Helper()
	var events []Event
	for _, line := range strings.Split(strings.TrimSpace(jsonl), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	return events
}

func stat(rep *Report, event string) Stat {
	for _, s := range rep.Stats {
		if s.Event == event {
			return s
		}
	}
	return Stat{}
}

func hasStat(rep *Report, event string) bool {
	return stat(rep, event).Count > 0
}
