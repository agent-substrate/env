# Substrate RL benchmark: system design and work spec

Status: draft. Owner: benchmarking. Scope: `internal/benchmarking/`,
`cmd/benchmarking/`, `benchmarking/`.

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
| Events | JSONL, one operation per line | load generator, SDK instrumentation, prewarm | report |
| Report | JSON, includes system description and validity | report | comparator, humans |

Schema changes to any artifact require a version field bump and a reader that
accepts the previous version.

## 4. Concepts

- **SUT**: the sandbox system behind the sandbox SDK.
- **Scenario**: one benchmark run. SUT config plus a list of jobs. Its hash is
  recorded in the system description.
- **Job**: one RL loop. Fields: id, kind (`simulated` | `real`), mode for real
  jobs (`measure` | `trace`), start offset, concurrency, park policy, dataset.
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
endpoint`, `Teardown()`. One implementation per SUT, each wrapping a Helm
chart. Writes the system description: commits, machine type, node count,
sandbox class, bucket region, scenario hash.

### 5.3 Job Runner
Owns one loop. Interface: `Setup`, `Start`, `Wait`, `Collect`, `Teardown`.

- **Simulated**: Setup bakes dataset and resolved schedule into the load
  generator image and creates an in-cluster Job. Collect copies events out.
  Teardown deletes leftover environments and the Job. Generalizes
  `benchmarking/swebench/run-in-cluster.sh`.
- **Real**: Setup deploys the trainer (GPUs, weights, inference server, SDK
  endpoint, trace flag only in trace mode). Collect pulls SDK events, trainer
  metrics and the trace if present. Teardown stops the trainer and releases
  GPUs. Lives beside the trainer it wraps.

### 5.4 Load generator
`cmd/benchmarking/swebench-loader`, one process per job. Seams: job spec in,
`Backend` and `Sandbox` interfaces as the SUT driver, events out. Modes:

- **eval**: closed loop; create, replay, delete at fixed concurrency. Exists.
- **steps**: a pool. Each step matches tasks to parked environments by image,
  resets the matched, creates the rest, parks or deletes the surplus. Churn is
  recorded. Replaces today's fixed fleet.
- **open-loop** (optional): Poisson arrivals regardless of completion, to find
  the throughput knee.

### 5.5 SDK instrumentation
Always-on lightweight events, one per call, cheap enough for measure mode. A
verbose trace flag adds payloads and timing detail. A converter turns a trace
into trajectories plus an explicit schedule.

### 5.6 Measurements
Per SUT, config-driven. Scrape server-side metrics for the run window and save
controller and API logs.

### 5.7 Orchestrator CLI
Phases: provision or accept an endpoint; resolve schedules; prewarm the union
of images across jobs, recorded as events; set up every job runner before any
start; start each job at its offset and wait; collect from runners and
measurements; tear down runners, then optionally the SUT; validate; report.

### 5.8 Report and comparator
Events carry run, job, step, environment, instance. The report gives per-job
percentile and per-step tables with task and distinct-image counts, a
cross-job timeline with latency split by how many other jobs were active,
churn per step, reward rate, validity and the system description. The
comparator diffs two reports against thresholds.

## 6. Validity rules

A run counts only if all hold. The report marks invalid runs and says why.

1. Warm or cold is declared and the prewarm events match the declaration.
2. Reward rate meets the configured threshold.
3. Task count meets the configured minimum.
4. Failed creates stay under the configured fraction.
5. Every job ran its full schedule.
6. No job in the scenario ran in trace mode.

## 7. Work items

| # | Item | Deliverable | Acceptance | Depends on |
|---|---|---|---|---|
| W1 | Artifact schemas | Go types with version fields for scenario, job, schedule, trace, events (extend), report (extend); JSON schema files; golden fixtures | round-trip tests; old events files still parse | none |
| W2 | Schedule resolver | explicit, sampled (seeded) and fixed sources resolve to one explicit form; saved as resolved schedule JSON | same seed gives identical output; fixture covers each source | W1 |
| W3 | Steps-mode pool | load generator matches tasks to parked environments by image, resets, creates, parks or deletes surplus; churn events | in-memory backend test with changing images per step; existing steps tests pass with a fixed schedule | W1, W2 |
| W4 | Job field in events and per-job report | events carry job id; report groups per job; cross-job timeline and interference split | report test over a two-job events fixture | W1 |
| W5 | Validity rules | validator over events plus scenario; report carries validity and reasons | one test per rule, pass and fail | W1, W4 |
| W6 | Comparator | `swebench-loader compare A.json B.json -thresholds T.yaml`; nonzero exit on regression | fixture pair with a planted regression | W4 |
| W7 | Simulated Job Runner | Go implementation of the runner interface over an in-cluster Job; replaces run-in-cluster.sh | smoke scenario with one job runs end to end against a cluster | W1, W2 |
| W8 | Provisioner interface and Substrate implementation | Helm-backed provision and teardown; system description written | provision, smoke run, teardown on a fresh namespace | W1 |
| W9 | Measurements | config-driven Prometheus queries and log capture for a run window | measurements collected for the smoke run and attached to the report | W8 |
| W10 | Orchestrator CLI | `cmd/benchmarking/bench run SCENARIO.yaml` driving phases in 5.7 | two simulated jobs with offsets run concurrently; report is valid | W2, W4, W5, W7, W8, W9 |
| W11 | SDK instrumentation | lightweight events always on; verbose trace flag; overhead measured | measure-mode overhead under an agreed bound; trace fixture produced | none |
| W12 | Trace converter | trace JSONL to trajectories plus explicit schedule | converted trace replays through the load generator with matching step counts and think times | W1, W2, W11 |
| W13 | Real Job Runner | runner interface over one trainer, measure and trace modes | one real job runs through the orchestrator; trace mode yields a trace W12 accepts | W10, W11 |
| W14 | Open-loop mode (optional) | Poisson arrival mode in the load generator | in-memory test shows arrivals independent of completion; report shows the knee | W3 |

Suggested parallel tracks: {W1, W11} first; then {W2, W4, W8} in parallel;
then {W3, W5, W6, W7, W9, W12}; then W10; then W13; W14 whenever.

## 8. Out of scope for now

Egress for sandboxes at scale, multiple SUTs in one scenario, and scheduling
runs on a cadence. Each is its own spec.
