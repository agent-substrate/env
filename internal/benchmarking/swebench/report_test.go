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
	"strings"
	"testing"
)

func TestPercentile(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if p := percentile(s, 0.5); p != 5 {
		t.Errorf("p50 = %v", p)
	}
	if p := percentile(s, 0.95); p != 10 {
		t.Errorf("p95 = %v", p)
	}
	if p := percentile(s, 1); p != 10 {
		t.Errorf("max = %v", p)
	}
	if p := percentile(nil, 0.5); p != 0 {
		t.Errorf("empty = %v", p)
	}
}

func TestSummarizeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	rec := NewRecorder(&buf)
	rec.Record(Event{RunID: "r", Event: EventCreate, Env: "e1", DurMS: 100, OK: true})
	rec.Record(Event{RunID: "r", Event: EventCreate, Env: "e2", DurMS: 300, OK: false, Err: "boom"})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "run_tests", DurMS: 50, OK: true, Exit: intp(0)})
	rec.Record(Event{RunID: "r", Event: EventStep, Name: "explore", DurMS: 5, OK: true, Exit: intp(0)})
	rec.Record(Event{RunID: "r", Event: EventTask, Step: 1, DurMS: 500, OK: true, Reward: intp(1)})
	rec.Record(Event{RunID: "r", Event: EventTask, Step: 1, DurMS: 600, OK: false})
	rec.Record(Event{RunID: "r", Event: EventWake, Step: 2, DurMS: 40, OK: true})
	rec.Record(Event{RunID: "r", Event: EventTask, Step: 2, DurMS: 700, OK: true, Reward: intp(0)})
	rec.Record(Event{RunID: "r", Event: EventBarrier, Step: 1, DurMS: 650, OK: true, Count: 2})
	rec.Record(Event{RunID: "r", Event: EventPark, Name: "suspend", Step: 1, DurMS: 90, OK: true, Count: 1})
	rec.Record(Event{RunID: "r", Event: EventRun, Name: "steps", DurMS: 2000, OK: false, Count: 3})
	if rec.Len() != 11 {
		t.Fatalf("recorded %d", rec.Len())
	}

	rep, err := Summarize(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if rep.RunID != "r" || rep.Mode != "steps" || rep.RunMS != 2000 {
		t.Fatalf("header: %+v", rep)
	}
	if rep.Tasks != 3 || rep.Failed != 1 || rep.Rewarded != 1 {
		t.Fatalf("tasks: %+v", rep)
	}
	find := func(event, name string) Stat {
		for _, s := range rep.Stats {
			if s.Event == event && s.Name == name {
				return s
			}
		}
		t.Fatalf("no stat %s/%s in %+v", event, name, rep.Stats)
		return Stat{}
	}
	if c := find(EventCreate, ""); c.Count != 2 || c.Failed != 1 || c.P50MS != 100 || c.MaxMS != 100 {
		t.Fatalf("create: %+v", c)
	}
	if s := find(EventStep, "run_tests"); s.Count != 1 || s.P50MS != 50 {
		t.Fatalf("step: %+v", s)
	}
	if len(rep.Steps) != 2 || rep.Steps[0].BarrierMS != 650 || rep.Steps[0].ParkMS != 90 || rep.Steps[1].WakeP50MS != 40 {
		t.Fatalf("steps: %+v", rep.Steps)
	}
	if len(rep.Errors) != 1 || rep.Errors[0].Err != "boom" || rep.Errors[0].Count != 1 {
		t.Fatalf("errors: %+v", rep.Errors)
	}
	var text bytes.Buffer
	rep.WriteText(&text)
	for _, want := range []string{"run r", "create", "run_tests", "barrier_s", "boom"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, text.String())
		}
	}
}
