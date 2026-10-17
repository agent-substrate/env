# Substrate RL benchmark: system design and work spec

Status: draft. Owner: benchmarking. Scope: `internal/benchmarking/`,
`cmd/benchmarking/`, `benchmarking/`.

## Overview (for humans)

This system benchmarks sandbox backends under RL training and evaluation
workloads. Components do not share code; they communicate strictly through
versioned JSON/JSONL/YAML file artifacts so that simulated load generators,
real RL trainers, and different sandbox backends (accessed via the sandbox SDK
or directly against the backend API) are drop-in replaceable.

```
Scenario.yaml + Dataset.jsonl
      │
      ▼
[ Orchestrator ] ──► [ Provisioner ] (setup SUT, prewarm, timed reap to zero)
      │
      ├──► [ Simulated Job Runner(s) ] (swebench-loader: eval | steps | open-loop)
      │                                         ▲
      │                                 (replays converted schedule + trajectories)
      │                                         │
      ├──► [ Real RL Job Runner(s) ] ──(trace mode)──► Trace.jsonl ──► [ Trace Converter ]
      │              │
      │       (measure mode)
      ▼              ▼
 Events.jsonl + Measurements (Prometheus / cluster health / hardware cost)
      │
      ▼
[ Report & Validator ] ──► Report.json ──► [ Comparator ] (regression gate)
```

- **Orchestrator & Lifecycle**: Given a `Scenario` (one SUT + one or more
  concurrent RL `Jobs`), the orchestrator provisions the SUT, resolves
  deterministic per-step task schedules, pre-warms initial pools, launches all
  jobs at their configured offsets, scrapes server/cluster telemetry, times a
  full cluster `Reap` back to zero resources, checks validity gates, and builds
  a `Report`.
- **Simulated vs. Real RL**:
  - **Simulated jobs** drive scripted `Trajectories` and `Resolved schedules`
    through `swebench-loader`, scaling across threads or in-cluster workers to
    saturate the SUT in single-pass (`eval`), multi-step pool (`steps` with
    park/wake/fork, trainer pauses, and next-step pool prefetch), or
    `open-loop` modes.
  - **Real jobs** run an actual RL trainer against the same SUT. In `measure`
    mode, lightweight always-on SDK instrumentation emits the same `Events`
    JSONL schema as the simulated loader. In `trace` mode, verbose SDK logs are
    captured and converted offline into `Trajectories` and a `Resolved schedule`
    that the simulated loader can replay repeatedly without accelerators.

---

## Agent Specification

This spec breaks the design into work items an agent can pick up alone. Each
item names its deliverable, acceptance criteria and dependencies. Items with
no shared dependency can run in parallel.

## 1. Principle

Reuse happens through file contracts, not shared code. Every component reads
or writes one of the artifacts in section 3 and nothing else. A component is
replaceable if the artifacts it touches are unchanged.

## 2. Two sibling benchmarks

| Benchmark | Driver | Output |
|---|---|---|
| Simulated RL | load generator replaying scripted workloads | formal numbers; fast, cheap, runs often |
| Real RL | an actual trainer against the same SUT | formal numbers in *measure* mode, or a trace in *trace* mode, never both in one job |

Both share the dataset manifest, events, report, system description and
validity rules. Each has its own setup and teardown.

## 3. Artifacts (the contracts)

| Artifact | Format | Written by | Read by |
|---|---|---|---|
| Scenario | YAML | operator | orchestrator |
| Dataset manifest | JSONL, one instance per line | dataset tooling | load generator, real runner, prewarm |
| Trajectory | YAML | operator or trace converter | load generator |
| Resolved schedule | JSON | orchestrator (simulated), trace converter (real) | load generator, report |
| Trace | JSONL, one SDK call per line | SDK verbose flag | trace converter |
| Events | JSONL, one operation per line | load generator, SDK instrumentation, prewarm, reaper | report |
| Report | JSON, includes system description and validity | report | comparator, humans |

Schema changes to any artifact require a version field bump and a reader that
accepts the previous version.

## 4. Concepts

- **SUT**: a sandbox backend accessed either through the sandbox SDK or
  directly against the backend API (to isolate and price the SDK layer).
- **Scenario**: one benchmark run. SUT config plus a list of jobs. Its hash is
  recorded in the system description.
- **Job**: one RL loop. Fields: id, kind (`simulated` | `real`), mode for real
  jobs (`measure` | `trace`), start offset, concurrency, step pause (simulated
  training and weight-transfer interval between steps), prefetch policy
  (whether next-step pools are warmed ahead of the step boundary), park policy
  (`suspend` | `pause` | `fork` | `delete` | `none`), dataset.
- **Workload**: dataset manifest, trajectories and a schedule.
- **Dataset manifest**: instances with a digest-pinned image, prompt material,
  grader inputs and the image-specific setup and reset steps.
- **Task**: an instance plus a trajectory. Simulated jobs are given the
  trajectory; real jobs produce it.
- **Schedule**: the tasks a simulated job runs at each step. Authored as
  explicit per-step lists, as a seeded sampler over a dataset and trajectory
  pool, or as a fixed set. Always resolved to explicit form and saved before
  the run. For a traced real job it is recovered afterward.

## 5. Components

### 5.1 Dataset tooling (exists)
`benchmarking/swebench/`: mirror images by digest, build the manifest,
maintain exclusion lists.

### 5.2 Provisioner
Owns the SUT shared by all jobs. Interface: `Provision(cluster, config) ->
endpoint`, `Reap(runID) -> ReapStats`, `Teardown()`. One implementation per
backend and access mode (via SDK or direct backend API), each wrapping a Helm
chart or manifest set. Writes the system description: cluster, backend and SDK
versions/commits, machine types, node counts, zone and network topology,
sandbox runtime class, sandbox resource footprint (CPU, memory, disk), bucket
region, scenario hash.

### 5.3 Job Runner
Owns one loop. Interface: `Setup`, `Start`, `Wait`, `Collect`, `Teardown`.

- **Simulated**: Setup stages the dataset manifest and resolved schedule and
  deploys the load generator in-cluster. Collect copies events out. Teardown
  deletes leftover environments and the runner resources. Generalizes
  `benchmarking/swebench/run-in-cluster.sh`.
- **Real**: Setup deploys the trainer (accelerators, weights, inference
  server, SDK endpoint, trace flag only in trace mode). Collect pulls SDK
  events, trainer step and throughput metrics, and the trace if present.
  Teardown stops the trainer and releases accelerators. Lives beside the
  trainer it wraps.

### 5.4 Load generator
`cmd/benchmarking/swebench-loader`. Seams: job spec in, `Backend` and
`Sandbox` interfaces as the SUT driver (supporting SDK and direct-backend
implementations, plus `create`, `fork`, `suspend`/`pause`, `wake`/`resume`,
`delete`, and batch pool pre-warming and retirement), events out. Scales
across threads or worker instances as needed so client-side load generation
and pre-warming are never the throughput bottleneck. Modes:

- **eval**: closed loop; create, replay, delete at fixed concurrency. Exists.
- **steps**: a pool across training steps separated by a configurable step
  pause. Each step matches tasks to parked environments by image, resets or
  forks the matched, creates the rest, and parks or deletes the surplus. When
  prefetch is enabled, uses the resolved schedule to pre-warm the next step's
  pools and retire the previous step's surplus, recording churn, lead margin
  (next step's pools ready minus step boundary) and release overlap. Replaces
  today's fixed fleet.
- **open-loop** (optional): Poisson arrivals regardless of completion, to find
  the throughput knee.

### 5.5 SDK instrumentation
Always-on lightweight events, one per call, cheap enough for measure mode.
Captures operation duration, sub-phase split (`claim` to `Ready` vs. `Ready`
to first command), acquisition source (`warm_pool`, `golden_restore`, `fork`,
`resume`, `cold_create`) and node image cache state (`warm` | `cold` when
reported by the backend). A verbose trace flag adds payloads and timing
detail. A converter turns a trace into trajectories plus an explicit schedule.

### 5.6 Measurements
Per SUT, config-driven. Scrapes server-side and cluster metrics for the run
window and saves controller and API logs:

- **Sandbox and lifecycle internals**: image pull latency, template and
  snapshot creation rates, per-node and per-pod readiness rates, active vs.
  idle sandboxes.
- **Cluster health**: node readiness transitions, network identity count and
  creation rate, API server and object-store rate-limit responses (HTTP 429s),
  database connection saturation, controller restarts and leader-election
  losses, queue admission latency.
- **Hardware utilization**: total and active sandboxes per node and per vCPU
  over the run window.

### 5.7 Orchestrator CLI
Phases: provision or accept an endpoint; resolve schedules; prewarm configured
initial images across jobs, recorded as events; set up every job runner before
any start; start each job at its offset and wait; collect from runners and
measurements; reap run resources (timed clear of claims, pools, templates,
sandboxes and pods to zero, recorded as events); tear down runners, then
optionally the SUT; validate; report.

### 5.8 Report and comparator
Events carry run, job, step, environment, instance, acquisition source and
node cache state. The report gives:

- **Per-job percentile (`p50`/`p90`/`p95`/`max`) and per-step tables**: TTFI
  (total and split by `claim`-to-`Ready` vs. `Ready`-to-first-command,
  acquisition source, and warm vs. cold cache), lifecycle operations (`create`,
  `fork`, `suspend`/`pause`, `wake`, `delete`), per-step barrier time, fleet
  park and release times, prefetch lead margin, churn per step, task and
  distinct-image counts, and reward rate.
- **Trainer metrics** (for real jobs): step wall-clock breakdown (sampling
  waits on sandbox acquire, command execution, and reward; training; weight
  transfer), token and FLOP throughput per accelerator, and reward convergence.
- **Cluster health, reap and hardware cost**: cluster health summary, reap
  wall time to zero and deletion rate, sandboxes (total and active) per node
  and per vCPU percentiles, and node-hours per completed task.
- **Multi-job interference**: a cross-job timeline with latency split by how
  many other jobs were active, plus run validity and the system description.

The comparator diffs two reports against thresholds.

## 6. Validity rules

A run counts only if all hold. The report marks invalid runs and says why.

1. Warm or cold is declared and the prewarm events match the declaration.
2. Reward rate meets the configured threshold.
3. Task count meets the configured minimum.
4. Failed creates stay under the configured fraction.
5. Every job ran its full schedule.
6. No job in the scenario ran in trace mode.
7. Reap reaches zero remaining sandboxes, pools, templates and claims within
   the reap timeout (zero orphans).

## 7. Agent guardrails and testing invariants

Agents implementing work items must obey these invariants; tests must enforce
them so an implementation cannot pass by fulfilling the letter of a work item
while breaking the system.

1. **Strict schema enforcement across languages**: Go readers must reject
   unknown JSON fields (`DisallowUnknownFields`) or validate against `W1`'s
   JSON Schema files. Python SDK event/trace emitters (`W11`) must validate
   their output against the exact same JSON Schema files from `W1`, never a
   hand-written copy.
2. **File-only boundaries (no in-memory shortcuts)**: The orchestrator,
   schedule resolver, trace converter, load generator, reporter and comparator
   must communicate strictly by reading and writing the section 3 files on
   disk, never by passing in-memory Go structs across stages. Dataset-specific
   setup, reset and grading logic must live in the dataset manifest and
   trajectory files, never hardcoded in the orchestrator, pool manager or
   reporter.
3. **Server truth for reap and measurements (no silent zeros)**:
   - `Reap` (`W8`, `W10`) must query the backend/cluster API for all live
     resources matching the run label or prefix; deleting only the handles the
     client remembered in memory is forbidden.
   - `Measurements` (`W9`) must distinguish `0` (measured zero errors) from
     `null`/error (metric missing or scrape failed). A failed scrape must fail
     measurement collection, never default to `0`.
4. **Zero extra RPCs on the instrumented path**: In `measure` mode, SDK
   instrumentation (`W11`) and backend drivers (`W8`) must extract attribution
   fields (`source`, `node_cache`, sub-phase timings) from existing response
   headers or payloads and buffer writes asynchronously. Issuing extra
   status/inspect RPCs or synchronous per-event disk flushes on the hot path is
   forbidden.
5. **Exact raw-event aggregation**: `Report` (`W4`) must compute percentiles
   (`p50`, `p90`, `p95`, `max`) over the merged raw event durations, never by
   averaging percentiles across steps, shards or jobs.
6. **Non-blocking prefetch and client self-benchmarking**: Next-step pool
   prefetch and previous-step retirement (`W3`) must run asynchronously off the
   step barrier's critical path. The load generator and event recorder must
   include a Go benchmark (`go test -bench`) against a zero-latency fake
   backend proving client-side overhead does not bottleneck high-concurrency
   bursts.
7. **End-to-end chain test without hand-crafted intermediate mocks**: A
   chain test must feed a trace emitted by `W11` into `W12` (converter) $\to$
   `W2` (resolver) $\to$ `W3` (load generator over an in-process gRPC/HTTP fake
   server) $\to$ `W5` (validator) $\to$ `W4` (report) $\to$ `W6` (comparator),
   verifying that concurrency curves, think times and step counts survive the
   full round trip.

## 8. Work items

| # | Item | Deliverable | Acceptance | Depends on |
|---|---|---|---|---|
| W1 | Artifact schemas | Go types with version fields for scenario, job, schedule, trace, events (extend with sub-phase, acquisition source, cache state, pool prefetch/retire, reap and trainer step events), report (extend with lifecycle, trainer, cluster health and hardware cost sections); JSON schema files; golden fixtures | round-trip tests with `DisallowUnknownFields`; schema validation helper usable from Go and Python; old events files still parse | none |
| W2 | Schedule resolver | explicit, sampled (seeded) and fixed sources resolve to one explicit form; saved as resolved schedule JSON | same seed gives identical output; fixture covers each source; validates against W1 schema | W1 |
| W3 | Steps-mode pool and prefetch | load generator matches tasks to parked environments by image, resets or forks, creates, parks or deletes surplus; supports configurable step pause, async next-step pool prefetch and previous-step retirement; emits churn, lead-margin and release-overlap events | in-memory backend test with changing images per step, step pause and async prefetch proving positive lead margin; `go test -bench` throughput test; existing steps tests pass | W1, W2 |
| W4 | Job field in events and extended report | events carry job id and attribution fields; report groups per job, computes exact percentiles from raw events, splits TTFI and lifecycle metrics by source/cache state, computes hardware cost (including node-hours per task) and cross-job interference split | report test over a multi-job, multi-step raw events and measurements fixture verifying exact percentiles and interference buckets | W1 |
| W5 | Validity rules | validator over events, measurements and scenario (including zero-orphan reap check and missing-measurement check); report carries validity and reasons | one test per rule, pass and fail | W1, W4 |
| W6 | Comparator | `swebench-loader compare A.json B.json -thresholds T.yaml`; nonzero exit on regression | fixture pair with a planted regression | W4 |
| W7 | Simulated Job Runner | Go implementation of the runner interface staging manifest/schedule files and deploying the load generator in-cluster without client-side throughput bottlenecks; replaces run-in-cluster.sh | smoke scenario with one job runs end to end against a cluster | W1, W2 |
| W8 | Provisioner, Reaper and Backend drivers | Helm/manifest provision, server-queried timed reap to zero and teardown; `Backend` drivers covering SDK and direct-API access across supported sandbox backends; system description written | hermetic fake-API unit tests plus cluster smoke run verifying server-side orphan count reaches zero | W1 |
| W9 | Measurements | config-driven Prometheus/cluster queries (image pull, readiness rates, node/identity/429/DB/controller/queue health, sandboxes per node and vCPU) and log capture for a run window; distinguishes `0` from missing/failed scrape | test against fake Prometheus HTTP server covering both valid series and scrape failure; smoke run attaches measurements to report | W1, W8 |
| W10 | Orchestrator CLI | `cmd/benchmarking/bench run SCENARIO.yaml` driving phases in 5.7 strictly via file artifacts, including timed reap | two simulated jobs with offsets run concurrently; reap converges to zero; report is valid | W2, W4, W5, W7, W8, W9 |
| W11 | SDK instrumentation | lightweight async-buffered events always on (with sub-phase and acquisition source fields from existing responses, zero extra RPCs); verbose trace flag; overhead benchmarked | emitted events/traces validate against W1 JSON schemas; microbenchmark proves measure-mode overhead stays under bound; trace fixture produced | W1 |
| W12 | Trace converter | trace JSONL to trajectories plus explicit schedule preserving concurrency profile, payload sizes and per-step inter-command think times | end-to-end chain test: W11 trace $\to$ W12 $\to$ W3 load generator reproduces step counts, concurrency histogram and think-time distribution | W1, W2, W3, W11 |
| W13 | Real Job Runner | runner interface over one trainer, measure and trace modes; exports trainer step breakdown and accelerator throughput | one real job runs through the orchestrator; trace mode yields a trace W12 converts and replays | W10, W11, W12 |
| W14 | Open-loop mode (optional) | Poisson arrival mode in the load generator | in-memory test shows arrivals independent of completion; report shows the knee | W3 |

## 9. Milestones (incremental delivery and verification)

Work items are delivered and integrated in five vertical milestones. Each
milestone unlocks a general class of benchmark scenarios and ends with an
end-to-end verification gate so integration, schema and cluster lifecycle
defects surface before scaling up complexity or hardware cost.

| Milestone | Functionality / Scenario Enabled | Work Items | End-to-End Verification Gate |
|---|---|---|---|
| **M1: Single-Job Step-Pool & Regression Gate (Core MVP)** | Deterministic multi-step simulated RL loops (with dynamic per-step image sets, environment reuse/churn, step pauses and async next-step pool prefetch) against an existing endpoint, with automated validity and regression diffing. | `W1`, `W2`, `W3`, `W4` (single-job & lifecycle tables), `W5` (event/schedule rules), `W6` | Resolve a seeded multi-step schedule with changing images per step $\to$ run `swebench-loader` in `steps` mode against a live endpoint $\to$ emit a valid `Report.json` with positive prefetch lead margin $\to$ pass `compare` against a baseline report. |
| **M2: Automated SUT Lifecycle, Server Telemetry & Backend Drivers** | Self-contained single-job runs with automated SUT provisioning, server-side cluster health and hardware-cost telemetry, timed cluster reap to zero orphans, and A/B comparisons across backends and access layers (SDK vs. direct API). | `W7`, `W8`, `W9`, plus `W4`/`W5` extensions for measurements, hardware cost and zero-orphan reap | Provision a SUT from scratch $\to$ run an in-cluster simulated job through both the SDK driver and the direct-backend driver $\to$ attach scraped `Measurements` $\to$ verify `Reap` reaches zero server-side orphans $\to$ compare SDK vs. direct reports. |
| **M3: Multi-Job Contention & Throughput Saturation** | Concurrent multi-loop RL workloads sharing one SUT (staggered or aligned step barriers, independent pool churn) to measure cross-job interference, plus open-loop arrival sweeps to find throughput knees. | `W10`, `W4` (cross-job timeline & interference split), `W14` | `bench run SCENARIO.yaml` executes multiple offset simulated jobs concurrently on a shared SUT, prewarms initial pools, produces a valid report with cross-job interference tables, and reaps the cluster to zero. |
| **M4: SDK Telemetry & Trace-to-Simulation Replay** | Always-on low-overhead SDK event logging (`measure` mode) and offline conversion of verbose SDK call traces (`trace` mode) into deterministic schedules and trajectories replayable by the simulated loader without accelerators. | `W11`, `W12` | Capture a verbose `Trace.jsonl` from an SDK workload $\to$ convert via `W12` into `Trajectory` YAMLs and a `Resolved schedule` $\to$ replay through `M3` (`swebench-loader`) and verify step counts, concurrency histogram and think-time distribution match the trace. |
| **M5: Live Trainer Benchmarking** | End-to-end real RL training loops orchestrated against the SUT in both `measure` mode (unified sandbox + trainer step/throughput reporting) and `trace` mode (feeding new real workloads into `M4`). | `W13` | Orchestrate one real trainer job in `measure` mode (yielding a valid `Report.json` with trainer step breakdown and accelerator throughput) and one in `trace` mode (whose trace converts and replays cleanly through `M4`). |

**Parallel execution & fail-fast ordering**:
- `M1` is the minimum viable product: it requires no cluster provisioner,
  Prometheus scraper, or accelerators to start benchmarking an existing sandbox
  endpoint.
- `W11` (SDK instrumentation) only depends on `W1` and can be developed in
  parallel with `M1`–`M3` so `M4` is ready as soon as `M1`'s loader pool (`W3`)
  lands.
- `M2` intentionally precedes `M3` so orphan leaks, rate limits (HTTP 429s) and
  missing telemetry are caught on a single in-cluster job before multiplying
  load across concurrent jobs.

## 10. Out of scope for now

Egress for sandboxes at scale, multiple SUTs in one scenario, and scheduling
runs on a cadence. Each is its own spec.
