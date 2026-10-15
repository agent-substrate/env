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
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"sigs.k8s.io/yaml"
)

//go:embed trajectories/*
var trajectoryFS embed.FS

// DefaultTrajectory is the built-in trajectory used when none is given.
const DefaultTrajectory = "swebench-gold"

// Duration is a time.Duration that decodes from a string such as "1s".
type Duration time.Duration

// UnmarshalJSON accepts a quoted Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return fmt.Errorf("duration must be a quoted string like \"500ms\": %s", string(b))
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration back as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(time.Duration(d).String())), nil
}

// WriteSpec puts a file into the sandbox. Content is a template over the
// Instance, so a step can write the gold patch or the eval script.
type WriteSpec struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// File names a file next to the trajectory whose contents become
	// Content when the trajectory is loaded: a built-in asset for built-in
	// trajectories, a path relative to the YAML file otherwise.
	File string `json:"file,omitempty"`
	// Mode is an octal file mode string; empty means 0644.
	Mode string `json:"mode,omitempty"`
}

// FileMode parses Mode.
func (w WriteSpec) FileMode() (fs.FileMode, error) {
	if w.Mode == "" {
		return 0o644, nil
	}
	v, err := strconv.ParseUint(w.Mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q: %w", w.Mode, err)
	}
	return fs.FileMode(v), nil
}

// Step is one action of a trajectory: exactly one of Run or Write. Run and
// Write.Content are text/template strings evaluated against the Instance.
type Step struct {
	Name  string     `json:"name"`
	Run   string     `json:"run,omitempty"`
	Write *WriteSpec `json:"write,omitempty"`
	// Think is the simulated model latency before the step (an RL rollout
	// waits for the policy between commands).
	Think Duration `json:"think,omitempty"`
	// Timeout bounds a Run step; zero means the runner's default.
	Timeout Duration `json:"timeout,omitempty"`
	// ExpectExit, when set, fails the task if the exit code differs. A
	// Reward step never fails the task on exit code: its exit code is the
	// reward.
	ExpectExit *int `json:"expect_exit,omitempty"`
	// Reward marks the step whose exit code decides the task reward:
	// 1 when it exits 0, else 0. At most one step per trajectory.
	Reward bool `json:"reward,omitempty"`
}

// Trajectory is the scripted rollout replayed in every environment.
type Trajectory struct {
	Name string `json:"name"`
	// Reset runs before Steps on every step after the first in steps mode,
	// so a rollout starts from the image's state again (the gold patch
	// cannot be applied twice).
	Reset []Step `json:"reset,omitempty"`
	Steps []Step `json:"steps"`
}

// Names lists the built-in trajectories.
func Names() []string {
	entries, err := fs.ReadDir(trajectoryFS, "trajectories")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			names = append(names, n)
		}
	}
	return names
}

// LoadTrajectory returns a built-in trajectory by name or one read from a
// file when source names an existing path. Empty means DefaultTrajectory.
func LoadTrajectory(source string) (*Trajectory, error) {
	if source == "" {
		source = DefaultTrajectory
	}
	if data, err := os.ReadFile(source); err == nil {
		dir := filepath.Dir(source)
		return parseWithAssets(data, func(name string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, name))
		})
	}
	data, err := trajectoryFS.ReadFile("trajectories/" + source + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("no file or built-in trajectory %q (built-in: %s)", source, strings.Join(Names(), ", "))
	}
	return parseWithAssets(data, func(name string) ([]byte, error) {
		return trajectoryFS.ReadFile("trajectories/" + name)
	})
}

// parseWithAssets parses the trajectory and resolves every write.file
// through read, so the loaded trajectory carries its contents.
func parseWithAssets(data []byte, read func(name string) ([]byte, error)) (*Trajectory, error) {
	t, err := ParseTrajectory(data)
	if err != nil {
		return nil, err
	}
	for _, steps := range [][]Step{t.Reset, t.Steps} {
		for i := range steps {
			w := steps[i].Write
			if w == nil || w.File == "" {
				continue
			}
			if strings.Contains(w.File, "/") || strings.Contains(w.File, "..") {
				return nil, fmt.Errorf("trajectory %s step %s: write.file must be a plain file name", t.Name, steps[i].Name)
			}
			b, err := read(w.File)
			if err != nil {
				return nil, fmt.Errorf("trajectory %s step %s: %w", t.Name, steps[i].Name, err)
			}
			w.Content = string(b)
			w.File = ""
		}
	}
	return t, nil
}

// ParseTrajectory decodes and validates a YAML trajectory.
func ParseTrajectory(data []byte) (*Trajectory, error) {
	var t Trajectory
	if err := yaml.UnmarshalStrict(data, &t); err != nil {
		return nil, fmt.Errorf("decode trajectory: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Validate checks the trajectory's shape without rendering it.
func (t *Trajectory) Validate() error {
	if t.Name == "" {
		return errors.New("trajectory: name is required")
	}
	if len(t.Steps) == 0 {
		return fmt.Errorf("trajectory %s: at least one step is required", t.Name)
	}
	rewards := 0
	for _, s := range t.Steps {
		if err := s.validate(); err != nil {
			return fmt.Errorf("trajectory %s: %w", t.Name, err)
		}
		if s.Reward {
			rewards++
		}
	}
	if rewards > 1 {
		return fmt.Errorf("trajectory %s: more than one reward step", t.Name)
	}
	for _, s := range t.Reset {
		if err := s.validate(); err != nil {
			return fmt.Errorf("trajectory %s reset: %w", t.Name, err)
		}
		if s.Reward {
			return fmt.Errorf("trajectory %s reset: step %s cannot be a reward step", t.Name, s.Name)
		}
	}
	return nil
}

func (s Step) validate() error {
	if s.Name == "" {
		return errors.New("step without a name")
	}
	switch {
	case s.Run != "" && s.Write != nil:
		return fmt.Errorf("step %s: run and write are exclusive", s.Name)
	case s.Run == "" && s.Write == nil:
		return fmt.Errorf("step %s: one of run or write is required", s.Name)
	case s.Write != nil && s.Write.Path == "":
		return fmt.Errorf("step %s: write.path is required", s.Name)
	case s.Write != nil && s.Write.File != "" && s.Write.Content != "":
		return fmt.Errorf("step %s: write.file and write.content are exclusive", s.Name)
	case s.Write != nil && (s.Reward || s.ExpectExit != nil):
		return fmt.Errorf("step %s: reward and expect_exit apply to run steps only", s.Name)
	}
	if s.Write != nil {
		if _, err := s.Write.FileMode(); err != nil {
			return fmt.Errorf("step %s: %w", s.Name, err)
		}
	}
	return nil
}

// HasReward reports whether a step decides the task reward.
func (t *Trajectory) HasReward() bool {
	for _, s := range t.Steps {
		if s.Reward {
			return true
		}
	}
	return false
}

// Render evaluates every template in the trajectory against in and returns
// the concrete copy to replay. The original is not modified.
func (t *Trajectory) Render(in Instance) (*Trajectory, error) {
	out := &Trajectory{Name: t.Name}
	var err error
	if out.Reset, err = renderSteps(t.Name, t.Reset, in); err != nil {
		return nil, err
	}
	if out.Steps, err = renderSteps(t.Name, t.Steps, in); err != nil {
		return nil, err
	}
	return out, nil
}

func renderSteps(tname string, steps []Step, in Instance) ([]Step, error) {
	out := make([]Step, len(steps))
	for i, s := range steps {
		r := s
		var err error
		if r.Run, err = renderText(s.Run, in); err != nil {
			return nil, fmt.Errorf("trajectory %s step %s run: %w", tname, s.Name, err)
		}
		if s.Write != nil {
			w := *s.Write
			if w.Content, err = renderText(s.Write.Content, in); err != nil {
				return nil, fmt.Errorf("trajectory %s step %s write: %w", tname, s.Name, err)
			}
			r.Write = &w
		}
		out[i] = r
	}
	return out, nil
}

// templateFuncs are available in step templates. json renders a value as
// JSON, for handing an instance's fields to a script in the sandbox.
var templateFuncs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
}

func renderText(text string, in Instance) (string, error) {
	if !strings.Contains(text, "{{") {
		return text, nil
	}
	tmpl, err := template.New("step").Option("missingkey=error").Funcs(templateFuncs).Parse(text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, in); err != nil {
		return "", err
	}
	return buf.String(), nil
}
