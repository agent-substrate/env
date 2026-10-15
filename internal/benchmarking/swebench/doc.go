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

// Package swebench is the Fast Substrate RL benchmark: a loader that drives
// real SWE-bench task images through the ate-env API the way a simulated RL
// job would, and records what every phase cost.
//
// Two shapes are implemented, both parameterized:
//
//   - eval: N images x T tasks each, at most W in flight. Every task creates
//     an environment from the image, replays a trajectory (explore, apply the
//     gold patch, run the failing tests), records the reward and deletes the
//     environment. This is the SWE-bench Verified evaluation shape.
//   - steps: N images x R rollouts, created once and driven through S
//     training steps. Each step replays the trajectory on every environment,
//     waits for the last one (the per-step barrier), then parks the fleet
//     (suspend, delete or nothing) before the next step wakes it again.
//
// Every create, wake, step, suspend, delete and barrier is one JSONL event;
// Report turns the file into percentiles. The layout mirrors substrate's
// internal/benchmarking/boomer so a user class can be lifted into that
// harness later; it is not imported because Go does not allow importing
// another module's internal packages.
package swebench
