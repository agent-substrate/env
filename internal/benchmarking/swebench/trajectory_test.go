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
	"strings"
	"testing"
	"time"
)

func TestBuiltinTrajectoriesLoadAndRender(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no built-in trajectories")
	}
	in := Instance{InstanceID: "x", Image: "reg/img@" + goodDigest, Patch: "PATCH", EvalScript: "EVAL"}
	for _, n := range names {
		tr, err := LoadTrajectory(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		r, err := tr.Render(in)
		if err != nil {
			t.Fatalf("%s render: %v", n, err)
		}
		if len(r.Steps) != len(tr.Steps) {
			t.Fatalf("%s: rendered %d steps, want %d", n, len(r.Steps), len(tr.Steps))
		}
	}
	def, err := LoadTrajectory("")
	if err != nil || def.Name != DefaultTrajectory {
		t.Fatalf("default: %v %v", def, err)
	}
	if !def.HasReward() {
		t.Fatal("default trajectory has no reward step")
	}
}

func TestRenderSubstitutesInstanceFields(t *testing.T) {
	tr, err := ParseTrajectory([]byte(`
name: t
steps:
  - name: w
    write: {path: /tmp/p, content: "{{.Patch}}", mode: "0755"}
  - name: r
    run: "echo {{.InstanceID}}"
    think: 250ms
    timeout: 1m
    reward: true
`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := tr.Render(Instance{InstanceID: "id-1", Patch: "line1\nline2"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Steps[0].Write.Content != "line1\nline2" || r.Steps[1].Run != "echo id-1" {
		t.Fatalf("render: %+v", r.Steps)
	}
	if tr.Steps[1].Run != "echo {{.InstanceID}}" {
		t.Fatal("Render must not modify the original")
	}
	if time.Duration(r.Steps[1].Think) != 250*time.Millisecond || time.Duration(r.Steps[1].Timeout) != time.Minute {
		t.Fatalf("durations: %+v", r.Steps[1])
	}
	if m, _ := r.Steps[0].Write.FileMode(); m != 0o755 {
		t.Fatalf("mode: %v", m)
	}
}

func TestRenderUnknownFieldFails(t *testing.T) {
	tr, err := ParseTrajectory([]byte("name: t\nsteps:\n  - name: r\n    run: \"{{.Nope}}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Render(Instance{}); err == nil {
		t.Fatal("unknown field must fail at render")
	}
}

func TestParseTrajectoryRejects(t *testing.T) {
	cases := map[string]string{
		"no name":       "steps:\n  - name: a\n    run: true\n",
		"no steps":      "name: t\n",
		"run and write": "name: t\nsteps:\n  - name: a\n    run: x\n    write: {path: /p}\n",
		"neither":       "name: t\nsteps:\n  - name: a\n",
		"two rewards":   "name: t\nsteps:\n  - {name: a, run: x, reward: true}\n  - {name: b, run: y, reward: true}\n",
		"reward reset":  "name: t\nreset:\n  - {name: a, run: x, reward: true}\nsteps:\n  - {name: b, run: y}\n",
		"write reward":  "name: t\nsteps:\n  - name: a\n    write: {path: /p}\n    reward: true\n",
		"bad mode":      "name: t\nsteps:\n  - name: a\n    write: {path: /p, mode: \"zz\"}\n",
		"bad think":     "name: t\nsteps:\n  - name: a\n    run: x\n    think: soon\n",
		"unknown field": "name: t\nsteps:\n  - name: a\n    run: x\n    bogus: 1\n",
	}
	for name, in := range cases {
		if _, err := ParseTrajectory([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if strings.Contains(err.Error(), "panic") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
