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

// Package swebench holds the data types of the Fast Substrate RL benchmark on
// SWE-bench Verified, and the runner that drives them through the ate-env API.
//
//   - Instance and LoadDataset: the JSONL manifest, one record per SWE-bench
//     instance (digest-pinned task image, gold patch, harness eval script,
//     the tests that decide the verdict, the repo's log parser).
//   - Trajectory: the scripted rollout a task replays (run a command or write
//     a file, with think time, timeout, expected exit and one reward step),
//     with setup steps that run once per environment and reset steps that
//     run before each later step. The built-in swebench-gold trajectory
//     applies the gold patch, runs the harness eval script and grades the
//     log with swebench-grade.py, a port of the SWE-bench harness's rules.
//   - Event and Recorder: one JSON line per operation of a run.
//   - Summarize and Build: the report over those events, with percentiles,
//     retry counts, a per-step table and the unresolved instances in
//     exclude-file format.
//
// The layout mirrors substrate's internal/benchmarking/boomer so a user class
// can be lifted into that harness later; it is not imported because Go does
// not allow importing another module's internal packages.
package swebench
