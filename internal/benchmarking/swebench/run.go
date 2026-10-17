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
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	env "github.com/agent-substrate/env/clients/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Mode selects the workload shape.
type Mode string

const (
	// ModeEval runs every (image, task) once: create, replay, delete.
	ModeEval Mode = "eval"
	// ModeSteps keeps a fleet across training steps and parks it in between.
	ModeSteps Mode = "steps"
)

// Park is what happens to the fleet between training steps.
type Park string

const (
	ParkNone    Park = "none"    // environments stay running
	ParkSuspend Park = "suspend" // SuspendEnvironment; the next command wakes it
	ParkDelete  Park = "delete"  // DeleteEnvironment; the next step creates again
)

// Config is one run. Every field that shapes the load is a parameter so the
// same binary serves the smoke test and the 500-wide run.
type Config struct {
	RunID      string
	Backend    Backend
	Instances  []Instance
	Trajectory *Trajectory
	Recorder   *Recorder
	Logger     *slog.Logger

	Mode Mode
	// Width is the most tasks in flight at once (the "500 wide").
	Width int
	// TasksPerImage is the eval mode fan-out per image.
	TasksPerImage int
	// Rollouts and Steps shape steps mode: Instances x Rollouts environments
	// driven through Steps training steps.
	Rollouts int
	Steps    int
	Park     Park

	// CreateTimeout bounds one create call; StepTimeout is the default for a
	// run step without its own timeout.
	CreateTimeout time.Duration
	StepTimeout   time.Duration
	// WakeBudget is how long the first command after a create or a park is
	// retried on transient errors: a wake that outlasts the router's parking
	// budget answers Unavailable, and a first create whose image is still
	// being golden-baked answers Unknown until the bake is done. WakeBackoff
	// is the initial pause between attempts; it doubles up to five times
	// that.
	WakeBudget  time.Duration
	WakeBackoff time.Duration
	// Keep leaves the environments in place when the run ends.
	Keep bool
}

// Summary is what a run produced, for the exit status and the log.
type Summary struct {
	Tasks    int
	Failed   int
	Rewarded int
	Duration time.Duration
}

func (c *Config) validate() error {
	switch {
	case c.RunID == "":
		return errors.New("run id is required")
	case !runIDPattern.MatchString(c.RunID):
		return fmt.Errorf("run id %q must be lowercase letters, digits and '-'", c.RunID)
	case len(c.RunID) > maxRunIDLen:
		return fmt.Errorf("run id %q is longer than %d characters; it prefixes every environment name", c.RunID, maxRunIDLen)
	case c.Backend == nil:
		return errors.New("backend is required")
	case len(c.Instances) == 0:
		return errors.New("no instances")
	case c.Trajectory == nil:
		return errors.New("trajectory is required")
	case c.Width < 1:
		return errors.New("width must be >= 1")
	}
	switch c.Mode {
	case ModeEval:
		if c.TasksPerImage < 1 {
			return errors.New("tasks per image must be >= 1")
		}
	case ModeSteps:
		if c.Rollouts < 1 || c.Steps < 1 {
			return errors.New("rollouts and steps must be >= 1")
		}
		switch c.Park {
		case ParkNone, ParkSuspend, ParkDelete:
		default:
			return fmt.Errorf("unknown park mode %q", c.Park)
		}
	default:
		return fmt.Errorf("unknown mode %q", c.Mode)
	}
	if n := c.longestName(); len(n) > maxNameLen {
		return fmt.Errorf("environment name %q has %d characters, more than Substrate's %d; "+
			"use a shorter run id, fewer rollouts or tasks per image, or fewer steps", n, len(n), maxNameLen)
	}
	return nil
}

// longestName is the longest environment name the run builds: EnvID for the
// largest replay index of each instance, plus the "-s<step>" suffix that a
// delete park gives the environments of later steps.
func (c *Config) longestName() string {
	k, suffix := c.TasksPerImage-1, ""
	if c.Mode == ModeSteps {
		k = c.Rollouts - 1
		if c.Park == ParkDelete && c.Steps > 1 {
			suffix = fmt.Sprintf("-s%d", c.Steps)
		}
	}
	longest := ""
	for _, in := range c.Instances {
		if n := EnvID(c.RunID, in.InstanceID, k) + suffix; len(n) > len(longest) {
			longest = n
		}
	}
	return longest
}

func (c *Config) defaults() {
	if c.Recorder == nil {
		c.Recorder = NewRecorder(nil)
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.CreateTimeout <= 0 {
		c.CreateTimeout = 5 * time.Minute
	}
	if c.StepTimeout <= 0 {
		c.StepTimeout = 10 * time.Minute
	}
	if c.WakeBudget <= 0 {
		c.WakeBudget = 3 * time.Minute
	}
	if c.WakeBackoff <= 0 {
		c.WakeBackoff = DefaultWakeBackoff
	}
}

// DefaultWakeBackoff is the initial pause between first-command retries.
const DefaultWakeBackoff = 2 * time.Second

// runIDPattern is Substrate's name rule (a k8s short name); the run id is a
// prefix of every environment name. maxRunIDLen caps the run id; validate
// checks the longest name the run builds against maxNameLen, because the
// instance ids, the replay index and the step suffix add to it.
var runIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	maxRunIDLen = 20
	maxNameLen  = 63
)

// cleanupTimeout bounds the deletes that run after a cancelled or finished
// run; they use a context that survives the cancellation so nothing leaks.
const cleanupTimeout = 10 * time.Minute

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// tail returns at most n trailing bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// environment is one sandbox and its progress. One goroutine owns it at a
// time; phases are separated by barriers, so it needs no lock.
type environment struct {
	// id is the current actor name; base is the name of the first one. A
	// recreate after a delete park gets a fresh name, since Substrate
	// deletes asynchronously and the old name is not free immediately.
	id   string
	base string
	inst Instance
	task int
	traj *Trajectory

	sb          Sandbox
	createStart time.Time
	// failed environments are skipped for the rest of a steps run.
	failed bool
}

type runner struct {
	cfg *Config
	rec *Recorder
	log *slog.Logger

	mu       sync.Mutex
	tasks    int
	failed   int
	rewarded int
}

// Run executes the configured workload and returns its summary. A non-nil
// error means the run could not be carried out; task failures are counted in
// the Summary and recorded as events, not returned.
func Run(ctx context.Context, cfg Config) (*Summary, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.defaults()
	r := &runner{cfg: &cfg, rec: cfg.Recorder, log: cfg.Logger}

	envs, err := r.plan()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	switch cfg.Mode {
	case ModeEval:
		r.runEval(ctx, envs)
	case ModeSteps:
		r.runSteps(ctx, envs)
	}
	dur := time.Since(start)
	s := &Summary{Tasks: r.tasks, Failed: r.failed, Rewarded: r.rewarded, Duration: dur}
	r.rec.Record(Event{RunID: cfg.RunID, Event: EventRun, Name: string(cfg.Mode), DurMS: ms(dur),
		OK: s.Failed == 0, Count: s.Tasks})
	return s, ctx.Err()
}

// plan renders the trajectory per instance and names every environment.
func (r *runner) plan() ([]*environment, error) {
	fanout := r.cfg.TasksPerImage
	if r.cfg.Mode == ModeSteps {
		fanout = r.cfg.Rollouts
	}
	envs := make([]*environment, 0, len(r.cfg.Instances)*fanout)
	for _, in := range r.cfg.Instances {
		traj, err := r.cfg.Trajectory.Render(in)
		if err != nil {
			return nil, err
		}
		for k := 0; k < fanout; k++ {
			id := EnvID(r.cfg.RunID, in.InstanceID, k)
			envs = append(envs, &environment{
				id:   id,
				base: id,
				inst: in,
				task: k,
				traj: traj,
			})
		}
	}
	return envs, nil
}

// EnvID derives a Substrate-valid name (lowercase alphanumerics and '-', at
// most 63 characters) from the run, the instance and the replay index. A
// truncated instance id gets a hash suffix so distinct instances stay
// distinct.
func EnvID(runID, instanceID string, k int) string {
	const maxInstance = 36
	b := strings.Builder{}
	for _, c := range strings.ToLower(instanceID) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	inst := strings.Trim(b.String(), "-")
	if len(inst) > maxInstance {
		h := fnv.New32a()
		_, _ = h.Write([]byte(instanceID))
		inst = fmt.Sprintf("%s-%08x", strings.Trim(inst[:maxInstance-9], "-"), h.Sum32())
	}
	return fmt.Sprintf("%s-%s-%d", runID, inst, k)
}

// forEach runs fn over every index with at most width in flight and waits
// for all of them. It stops launching when ctx is done.
func forEach(ctx context.Context, n, width int, fn func(i int)) {
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// runEval is the evaluation shape: every environment is created, replayed
// once and deleted, Width at a time.
func (r *runner) runEval(ctx context.Context, envs []*environment) {
	var done int
	forEach(ctx, len(envs), r.cfg.Width, func(i int) {
		e := envs[i]
		if r.create(ctx, e, 0) {
			r.replay(ctx, e, 0)
		}
		if !r.cfg.Keep {
			// Delete with a context that survives a cancelled run, or a
			// Ctrl-C at width 500 leaves 500 environments behind.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			r.remove(cleanup, e, 0)
			cancel()
		}
		r.mu.Lock()
		done++
		if done%100 == 0 || done == len(envs) {
			r.log.Info("eval progress", slog.Int("done", done), slog.Int("total", len(envs)),
				slog.Int("failed", r.failed), slog.Int("rewarded", r.rewarded))
		}
		r.mu.Unlock()
	})
}

// runSteps is the training shape: the fleet is created once, every step
// replays the trajectory on all of it behind a barrier, and the fleet is
// parked between steps.
func (r *runner) runSteps(ctx context.Context, envs []*environment) {
	forEach(ctx, len(envs), r.cfg.Width, func(i int) { r.create(ctx, envs[i], 0) })
	r.log.Info("fleet created", slog.Int("environments", len(envs)), slog.Int("failed", r.countFailed(envs)))

	for step := 1; step <= r.cfg.Steps; step++ {
		if ctx.Err() != nil {
			break
		}
		if step > 1 && r.cfg.Park == ParkDelete {
			forEach(ctx, len(envs), r.cfg.Width, func(i int) {
				if !envs[i].failed {
					r.create(ctx, envs[i], step)
				}
			})
		}
		barrier := time.Now()
		forEach(ctx, len(envs), r.cfg.Width, func(i int) {
			if !envs[i].failed {
				r.replay(ctx, envs[i], step)
			}
		})
		live := len(envs) - r.countFailed(envs)
		r.rec.Record(Event{RunID: r.cfg.RunID, Event: EventBarrier, Step: step, DurMS: ms(time.Since(barrier)),
			OK: ctx.Err() == nil, Count: live})
		r.log.Info("step done", slog.Int("step", step), slog.Duration("barrier", time.Since(barrier)),
			slog.Int("live", live), slog.Int("failed", r.failed), slog.Int("rewarded", r.rewarded))

		if step == r.cfg.Steps || r.cfg.Park == ParkNone || ctx.Err() != nil {
			continue
		}
		parkStart := time.Now()
		forEach(ctx, len(envs), r.cfg.Width, func(i int) {
			e := envs[i]
			if e.failed {
				return
			}
			switch r.cfg.Park {
			case ParkSuspend:
				r.suspend(ctx, e, step)
			case ParkDelete:
				r.remove(ctx, e, step)
			}
		})
		r.rec.Record(Event{RunID: r.cfg.RunID, Event: EventPark, Name: string(r.cfg.Park), Step: step,
			DurMS: ms(time.Since(parkStart)), OK: ctx.Err() == nil, Count: live})
	}

	if r.cfg.Keep {
		return
	}
	// Tear down with a context that survives a cancelled run so nothing
	// leaks on Ctrl-C.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	forEach(cleanup, len(envs), r.cfg.Width, func(i int) {
		if envs[i].sb != nil {
			r.remove(cleanup, envs[i], r.cfg.Steps)
		}
	})
}

func (r *runner) countFailed(envs []*environment) int {
	n := 0
	for _, e := range envs {
		if e.failed {
			n++
		}
	}
	return n
}

// create makes the environment and records the call. It returns false and
// marks the environment failed when the create did not succeed.
func (r *runner) create(ctx context.Context, e *environment, step int) bool {
	if step > 1 {
		e.id = fmt.Sprintf("%s-s%d", e.base, step)
	}
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CreateTimeout)
	defer cancel()
	e.createStart = time.Now()
	sb, err := r.cfg.Backend.Create(cctx, e.id, e.inst.Image)
	ev := r.event(EventCreate, e, step, time.Since(e.createStart), err)
	r.rec.Record(ev)
	if err != nil {
		e.failed = true
		// A task that never got an environment is still a failed task, in
		// the report as well as in the summary.
		task := r.event(EventTask, e, step, 0, err)
		r.rec.Record(task)
		r.countTask(e, step, false, nil)
		r.log.Warn("create failed", slog.String("env", e.id), slog.String("err", err.Error()))
		// The server may still finish a create whose call failed or timed
		// out on our side; delete by name so it cannot leak or collide.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if derr := r.cfg.Backend.Delete(cleanup, e.id); derr != nil && !isNotFound(derr) {
			r.log.Warn("delete after failed create", slog.String("env", e.id), slog.String("err", derr.Error()))
		}
		return false
	}
	e.sb = sb
	return true
}

// replay runs the trajectory once on e and records one task event. A fresh
// environment (its first step, or any step after a delete park) runs the
// Setup steps first; one that survived a park runs the Reset steps first.
func (r *runner) replay(ctx context.Context, e *environment, step int) {
	fresh := step <= 1 || r.cfg.Park == ParkDelete
	prefix := e.traj.Reset
	if fresh {
		prefix = e.traj.Setup
	}
	steps := append(append([]Step{}, prefix...), e.traj.Steps...)
	// The first instruction of a task either follows a create (eval, or a
	// delete park) or a park (suspend); its wall time is the TTFI or the
	// wake, and it is the one that may find the environment not yet
	// serving.
	first := true
	firstEvent := EventTTFI
	firstStart := e.createStart
	if step > 1 && r.cfg.Park != ParkDelete {
		firstEvent = EventWake
		firstStart = time.Time{}
	}

	taskStart := time.Now()
	ok := true
	var reward *int
	for _, s := range steps {
		// No think time before the first command: it would land in the
		// ttfi or wake measurement, and the "model" has nothing to think
		// about before it has seen the environment.
		if s.Think > 0 && !first {
			select {
			case <-ctx.Done():
				ok = false
			case <-time.After(time.Duration(s.Think)):
			}
			if !ok {
				break
			}
		}
		stepStart := time.Now()
		if firstStart.IsZero() {
			firstStart = stepStart
		}
		res, attempts, err := r.doStep(ctx, e, s, first)
		dur := time.Since(stepStart)
		if first {
			fe := r.event(firstEvent, e, step, time.Since(firstStart), err)
			fe.Attempts = attempts
			r.rec.Record(fe)
			first = false
		}
		ev := r.event(EventStep, e, step, dur, err)
		ev.Name = s.Name
		if s.Run != "" && err == nil {
			ev.Exit = intp(res.ExitCode)
		}
		switch {
		case err != nil:
			ok = false
		case s.Reward:
			reward = intp(0)
			if res.ExitCode == 0 {
				*reward = 1
			}
			ev.Reward = reward
			// The grader's verdict is the last line of a reward step's
			// output; keep enough of it for the report to read it back.
			ev.Tail = tail(res.Stdout+res.Stderr, 4000)
		case s.ExpectExit != nil && res.ExitCode != *s.ExpectExit:
			ok = false
			ev.OK = false
			ev.Err = fmt.Sprintf("exit %d, expected %d", res.ExitCode, *s.ExpectExit)
			ev.Tail = tail(res.Stdout+res.Stderr, 2000)
		}
		r.rec.Record(ev)
		if !ok {
			r.log.Warn("step failed", slog.String("env", e.id), slog.String("step", s.Name), slog.String("err", ev.Err))
			break
		}
	}
	if ok && reward == nil && e.traj.HasReward() {
		ok = false
	}
	ev := r.event(EventTask, e, step, time.Since(taskStart), nil)
	ev.OK = ok
	ev.Reward = reward
	r.rec.Record(ev)
	r.countTask(e, step, ok, reward)
	if !ok && r.cfg.Mode == ModeSteps {
		e.failed = true
	}
}

// doStep performs one step and reports how many attempts it took. The
// first step of a task retries transient errors within WakeBudget: the
// environment may still be waking, or its image may still be baking.
func (r *runner) doStep(ctx context.Context, e *environment, s Step, first bool) (Result, int, error) {
	deadline := time.Now().Add(r.cfg.WakeBudget)
	wait := r.cfg.WakeBackoff
	var (
		res Result
		err error
	)
	for attempt := 1; ; attempt++ {
		if s.Write != nil {
			mode, _ := s.Write.FileMode()
			err = e.sb.WriteFile(ctx, s.Write.Path, []byte(s.Write.Content), mode)
		} else {
			timeout := time.Duration(s.Timeout)
			if timeout <= 0 {
				timeout = r.cfg.StepTimeout
			}
			sctx, cancel := context.WithTimeout(ctx, timeout+time.Minute)
			res, err = e.sb.Run(sctx, s.Run, timeout)
			cancel()
		}
		if err == nil || !first || !isTransient(err) || ctx.Err() != nil || time.Now().Add(wait).After(deadline) {
			return res, attempt, err
		}
		select {
		case <-ctx.Done():
			return res, attempt, err
		case <-time.After(wait):
		}
		wait = min(wait*2, 5*r.cfg.WakeBackoff)
	}
}

// isNotFound reports whether a delete found nothing to delete.
func isNotFound(err error) bool {
	if errors.Is(err, env.ErrNotFound) {
		return true
	}
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.NotFound
}

// isTransient reports whether a first-command error is worth another try.
// Unavailable is the router giving up on a wake; DeadlineExceeded, Unknown,
// Internal and Aborted are what a resume that races the golden bake or a
// worker restart surfaces as. Argument and precondition errors are not
// retried.
func isTransient(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Unknown, codes.Internal, codes.Aborted:
		return true
	}
	return false
}

func (r *runner) suspend(ctx context.Context, e *environment, step int) {
	start := time.Now()
	err := e.sb.Suspend(ctx)
	r.rec.Record(r.event(EventSuspend, e, step, time.Since(start), err))
	if err != nil {
		r.log.Warn("suspend failed", slog.String("env", e.id), slog.String("err", err.Error()))
	}
}

func (r *runner) remove(ctx context.Context, e *environment, step int) {
	if e.sb == nil {
		return
	}
	start := time.Now()
	err := e.sb.Delete(ctx)
	r.rec.Record(r.event(EventDelete, e, step, time.Since(start), err))
	if err != nil {
		// Keep e.sb for the final cleanup, and take the environment out of
		// later steps: a delete park would otherwise create the next name and
		// overwrite the only handle to this one, which then leaks.
		e.failed = true
		r.log.Warn("delete failed; the environment leaves the run and is deleted at the end",
			slog.String("env", e.id), slog.String("err", err.Error()))
		return
	}
	e.sb = nil
}

func (r *runner) event(kind string, e *environment, step int, d time.Duration, err error) Event {
	ev := Event{RunID: r.cfg.RunID, Event: kind, Env: e.id, Instance: e.inst.InstanceID,
		Step: step, Task: e.task, DurMS: ms(d), OK: err == nil}
	if err != nil {
		ev.Err = err.Error()
	}
	return ev
}

func (r *runner) countTask(e *environment, step int, ok bool, reward *int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks++
	if !ok {
		r.failed++
	}
	if reward != nil && *reward == 1 {
		r.rewarded++
	}
}
