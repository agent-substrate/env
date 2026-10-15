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
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Event names. Every timed operation of a run is one of these.
const (
	EventCreate  = "create"       // CreateEnvironment call
	EventTTFI    = "ttfi"         // create start to the first command's return
	EventWake    = "wake"         // first command after a park, including the wake it triggers
	EventStep    = "step"         // one trajectory step (name is the step name)
	EventTask    = "task"         // one trajectory replay end to end
	EventSuspend = "suspend"      // SuspendEnvironment call
	EventDelete  = "delete"       // DeleteEnvironment call
	EventBarrier = "step_barrier" // steps mode: first task start to last task end in a training step
	EventPark    = "park"         // steps mode: parking the whole fleet between steps
	EventRun     = "run"          // whole run
)

// Event is one JSONL record. Dur is wall time as seen by the loader; it
// includes the network and the API server, which is what a trainer sees.
type Event struct {
	Time     time.Time `json:"ts"`
	RunID    string    `json:"run"`
	Event    string    `json:"event"`
	Name     string    `json:"name,omitempty"`
	Env      string    `json:"env,omitempty"`
	Instance string    `json:"instance,omitempty"`
	// Step is the training step in steps mode (1-based); 0 in eval mode.
	Step int `json:"step,omitempty"`
	// Task numbers the replays of one instance (eval: task index; steps:
	// rollout index).
	Task   int     `json:"task,omitempty"`
	DurMS  float64 `json:"dur_ms"`
	OK     bool    `json:"ok"`
	Exit   *int    `json:"exit,omitempty"`
	Reward *int    `json:"reward,omitempty"`
	Err    string  `json:"err,omitempty"`
	// Count is the number of tasks or environments an aggregate event covers.
	Count int `json:"count,omitempty"`
	// Tail is the last bytes of a failed or reward step's output.
	Tail string `json:"tail,omitempty"`
	// Attempts is how many tries a ttfi or wake took; above 1 means the
	// environment was not serving when first asked.
	Attempts int `json:"attempts,omitempty"`
}

// Recorder writes events as JSON lines. It is safe for concurrent use.
type Recorder struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder
	n   int
}

// NewRecorder writes to w; a nil w discards events.
func NewRecorder(w io.Writer) *Recorder {
	if w == nil {
		w = io.Discard
	}
	return &Recorder{w: w, enc: json.NewEncoder(w)}
}

// Record writes one event, stamping the time if unset.
func (r *Recorder) Record(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.enc.Encode(e)
	r.n++
}

// Len is the number of events written so far.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func intp(v int) *int { return &v }

// tail returns at most n trailing bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
