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
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// Stat summarizes one kind of event.
type Stat struct {
	Event  string  `json:"event"`
	Name   string  `json:"name,omitempty"`
	Count  int     `json:"count"`
	Failed int     `json:"failed"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	MaxMS  float64 `json:"max_ms"`
}

// StepStat is one training step in steps mode.
type StepStat struct {
	Step      int     `json:"step"`
	Tasks     int     `json:"tasks"`
	Failed    int     `json:"failed"`
	Rewarded  int     `json:"rewarded"`
	BarrierMS float64 `json:"barrier_ms"`
	ParkMS    float64 `json:"park_ms"`
	WakeP50MS float64 `json:"wake_p50_ms"`
	WakeP95MS float64 `json:"wake_p95_ms"`
}

// Report is the summary of one run's JSONL, stable enough to diff between
// runs.
type Report struct {
	RunID      string     `json:"run"`
	Mode       string     `json:"mode,omitempty"`
	RunMS      float64    `json:"run_ms,omitempty"`
	Tasks      int        `json:"tasks"`
	Failed     int        `json:"failed"`
	Rewarded   int        `json:"rewarded"`
	RewardRate float64    `json:"reward_rate"`
	Stats      []Stat     `json:"stats"`
	Steps      []StepStat `json:"steps,omitempty"`
	// Errors counts distinct error strings, most frequent first.
	Errors []ErrorCount `json:"errors,omitempty"`
}

// ErrorCount is one error string and how often it occurred.
type ErrorCount struct {
	Err   string `json:"err"`
	Count int    `json:"count"`
}

// Summarize reads JSONL events and builds the report.
func Summarize(r io.Reader) (*Report, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var events []Event
	for sc.Scan() {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, fmt.Errorf("parse event: %w", err)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return Build(events), nil
}

// Build computes the report from events in memory.
func Build(events []Event) *Report {
	rep := &Report{}
	type key struct{ event, name string }
	durs := map[key][]float64{}
	failed := map[key]int{}
	var order []key
	steps := map[int]*StepStat{}
	wakes := map[int][]float64{}
	errs := map[string]int{}

	for _, e := range events {
		if rep.RunID == "" {
			rep.RunID = e.RunID
		}
		k := key{e.Event, ""}
		if e.Event == EventStep || e.Event == EventPark {
			k.name = e.Name
		}
		if _, ok := durs[k]; !ok {
			order = append(order, k)
			durs[k] = nil
		}
		if e.OK {
			durs[k] = append(durs[k], e.DurMS)
		} else {
			failed[k]++
			if e.Err != "" {
				errs[e.Err]++
			}
		}
		switch e.Event {
		case EventRun:
			rep.Mode = e.Name
			rep.RunMS = e.DurMS
		case EventTask:
			rep.Tasks++
			if !e.OK {
				rep.Failed++
			}
			if e.Reward != nil && *e.Reward == 1 {
				rep.Rewarded++
			}
			if e.Step > 0 {
				s := stepStat(steps, e.Step)
				s.Tasks++
				if !e.OK {
					s.Failed++
				}
				if e.Reward != nil && *e.Reward == 1 {
					s.Rewarded++
				}
			}
		case EventBarrier:
			stepStat(steps, e.Step).BarrierMS = e.DurMS
		case EventPark:
			stepStat(steps, e.Step).ParkMS = e.DurMS
		case EventWake:
			if e.OK {
				wakes[e.Step] = append(wakes[e.Step], e.DurMS)
			}
		}
	}
	if rep.Tasks > 0 {
		rep.RewardRate = float64(rep.Rewarded) / float64(rep.Tasks)
	}
	for _, k := range order {
		d := durs[k]
		sort.Float64s(d)
		rep.Stats = append(rep.Stats, Stat{Event: k.event, Name: k.name, Count: len(d) + failed[k], Failed: failed[k],
			P50MS: percentile(d, 0.50), P95MS: percentile(d, 0.95), MaxMS: percentile(d, 1)})
	}
	for step, s := range steps {
		w := wakes[step]
		sort.Float64s(w)
		s.WakeP50MS = percentile(w, 0.5)
		s.WakeP95MS = percentile(w, 0.95)
		rep.Steps = append(rep.Steps, *s)
	}
	sort.Slice(rep.Steps, func(i, j int) bool { return rep.Steps[i].Step < rep.Steps[j].Step })
	for msg, n := range errs {
		rep.Errors = append(rep.Errors, ErrorCount{Err: msg, Count: n})
	}
	sort.Slice(rep.Errors, func(i, j int) bool {
		if rep.Errors[i].Count != rep.Errors[j].Count {
			return rep.Errors[i].Count > rep.Errors[j].Count
		}
		return rep.Errors[i].Err < rep.Errors[j].Err
	})
	return rep
}

func stepStat(m map[int]*StepStat, step int) *StepStat {
	s, ok := m[step]
	if !ok {
		s = &StepStat{Step: step}
		m[step] = s
	}
	return s
}

// percentile of a sorted slice by nearest rank; 0 for an empty slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	i := int(p*float64(len(sorted))+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// WriteText prints the report as tables.
func (rep *Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "run %s mode=%s tasks=%d failed=%d rewarded=%d reward_rate=%.3f run=%.1fs\n\n",
		rep.RunID, rep.Mode, rep.Tasks, rep.Failed, rep.Rewarded, rep.RewardRate, rep.RunMS/1000)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "event\tname\tcount\tfailed\tp50_ms\tp95_ms\tmax_ms")
	for _, s := range rep.Stats {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%.0f\t%.0f\t%.0f\n", s.Event, s.Name, s.Count, s.Failed, s.P50MS, s.P95MS, s.MaxMS)
	}
	tw.Flush()
	if len(rep.Steps) > 0 {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "step\ttasks\tfailed\trewarded\tbarrier_s\tpark_s\twake_p50_ms\twake_p95_ms")
		for _, s := range rep.Steps {
			fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%.1f\t%.1f\t%.0f\t%.0f\n", s.Step, s.Tasks, s.Failed, s.Rewarded,
				s.BarrierMS/1000, s.ParkMS/1000, s.WakeP50MS, s.WakeP95MS)
		}
		tw.Flush()
	}
	if len(rep.Errors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "errors:")
		for i, e := range rep.Errors {
			if i == 10 {
				fmt.Fprintf(w, "  ... %d more\n", len(rep.Errors)-10)
				break
			}
			msg := strings.ReplaceAll(e.Err, "\n", " ")
			if len(msg) > 160 {
				msg = msg[:160] + "..."
			}
			fmt.Fprintf(w, "  %5d  %s\n", e.Count, msg)
		}
	}
}
