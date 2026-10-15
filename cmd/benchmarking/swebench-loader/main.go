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

// swebench-loader drives SWE-bench task images through the ate-env API as a
// simulated RL job and writes one JSONL event per operation.
//
//	swebench-loader run    -dataset manifest.jsonl -mode eval  -images 500 -tasks-per-image 6 -width 500 -out run.jsonl
//	swebench-loader run    -dataset manifest.jsonl -mode steps -images 500 -rollouts 2 -steps 8 -park suspend -out run.jsonl
//	swebench-loader report run.jsonl [-json summary.json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/swebench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: swebench-loader run [flags] | swebench-loader report FILE [-json OUT]")
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var (
		endpoint      = fs.String("endpoint", "localhost:7777", "ate-env API address (host:port).")
		atespace      = fs.String("atespace", "ate-env", "Atespace the environments are created in.")
		baseTemplate  = fs.String("base-template", "default-template", "Base ActorTemplate the per-image templates derive from (ate-env manifest template).")
		dataset       = fs.String("dataset", "", "JSONL manifest from benchmarking/swebench/make-dataset.py; a path or an http(s) URL. Required.")
		images        = fs.Int("images", 0, "Use the first N instances of the dataset; 0 means all.")
		instances     = fs.String("instances", "", "Comma-separated instance ids to use instead of the first N.")
		trajectory    = fs.String("trajectory", swebench.DefaultTrajectory, fmt.Sprintf("Trajectory file or built-in name (%s).", strings.Join(swebench.Names(), ", ")))
		mode          = fs.String("mode", string(swebench.ModeEval), "eval (images x tasks-per-image, each create/replay/delete) or steps (images x rollouts kept across steps).")
		width         = fs.Int("width", 50, "Most tasks in flight at once.")
		tasksPerImage = fs.Int("tasks-per-image", 1, "eval: replays per image.")
		rollouts      = fs.Int("rollouts", 2, "steps: environments per image.")
		steps         = fs.Int("steps", 8, "steps: training steps.")
		park          = fs.String("park", string(swebench.ParkSuspend), "steps: what happens between steps: suspend, delete or none.")
		createTimeout = fs.Duration("create-timeout", 5*time.Minute, "Deadline for one create call.")
		stepTimeout   = fs.Duration("step-timeout", 10*time.Minute, "Default deadline for a run step without its own timeout.")
		wakeRetries   = fs.Int("wake-retries", 10, "Retries of a task's first command while the API reports the environment unavailable.")
		wakeBackoff   = fs.Duration("wake-backoff", 2*time.Second, "Pause between wake retries.")
		runID         = fs.String("run-id", "", "Run id, prefix of every environment name; default swe-<unix time>.")
		out           = fs.String("out", "", "JSONL events file; default <run-id>.jsonl in the working directory.")
		keep          = fs.Bool("keep", false, "Leave the environments in place when the run ends.")
		dryRun        = fs.Bool("dry-run", false, "Load and render everything, print the plan and exit without touching the API.")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataset == "" {
		return fmt.Errorf("-dataset is required")
	}
	if *runID == "" {
		*runID = fmt.Sprintf("swe-%d", time.Now().Unix())
	}
	if *out == "" {
		*out = *runID + ".jsonl"
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	all, err := swebench.LoadDataset(*dataset)
	if err != nil {
		return err
	}
	var ids []string
	if *instances != "" {
		ids = strings.Split(*instances, ",")
	}
	selected, err := swebench.Select(all, ids, *images)
	if err != nil {
		return err
	}
	traj, err := swebench.LoadTrajectory(*trajectory)
	if err != nil {
		return err
	}
	for _, in := range selected {
		if _, err := traj.Render(in); err != nil {
			return err
		}
	}

	fanout := *tasksPerImage
	if swebench.Mode(*mode) == swebench.ModeSteps {
		fanout = *rollouts
	}
	log.Info("plan", slog.String("run", *runID), slog.String("mode", *mode), slog.Int("images", len(selected)),
		slog.Int("environments", len(selected)*fanout), slog.Int("width", *width), slog.Int("steps", *steps),
		slog.String("park", *park), slog.String("trajectory", traj.Name), slog.String("out", *out))
	if *dryRun {
		return nil
	}

	backend, err := swebench.NewEnvBackend(*endpoint, *atespace, *baseTemplate)
	if err != nil {
		return err
	}
	defer backend.Close()

	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sum, err := swebench.Run(ctx, swebench.Config{
		RunID:         *runID,
		Backend:       backend,
		Instances:     selected,
		Trajectory:    traj,
		Recorder:      swebench.NewRecorder(f),
		Logger:        log,
		Mode:          swebench.Mode(*mode),
		Width:         *width,
		TasksPerImage: *tasksPerImage,
		Rollouts:      *rollouts,
		Steps:         *steps,
		Park:          swebench.Park(*park),
		CreateTimeout: *createTimeout,
		StepTimeout:   *stepTimeout,
		WakeRetries:   *wakeRetries,
		WakeBackoff:   *wakeBackoff,
		Keep:          *keep,
	})
	if sum != nil {
		log.Info("run finished", slog.Int("tasks", sum.Tasks), slog.Int("failed", sum.Failed),
			slog.Int("rewarded", sum.Rewarded), slog.Duration("duration", sum.Duration))
	}
	if err != nil {
		return err
	}
	if sum.Failed > 0 {
		return fmt.Errorf("%d of %d tasks failed", sum.Failed, sum.Tasks)
	}
	return nil
}

func reportCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("report needs the JSONL file")
	}
	path := args[0]
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	jsonOut := fs.String("json", "", "Also write the summary as JSON to this path.")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	rep, err := swebench.Summarize(f)
	if err != nil {
		return err
	}
	rep.WriteText(os.Stdout)
	if *jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(*jsonOut, b, 0o644)
	}
	return nil
}
