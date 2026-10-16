# GRPO on Substrate: a real RL loop with actors as the reward executor

Port of the agent-sandbox stress harness trainer (`vicentefb/agent-sandbox#7`,
`examples/stress/snapshot_ray/harness/grpo_sandbox_train.py`) to Substrate's env client.
TRL's GRPOTrainer trains Qwen2.5-0.5B on MBPP; each completion is executed inside a gVisor
actor and the reward is the fraction of the task's asserts that pass. CPU only.

What changed vs. the original: only the seven sandbox calls (see the module docstring).
What is new: a `SMOKE=1` mode that checks the wiring without loading a model, a JSONL record
of every create / boot / eval / suspend / wake / heal with wall time, and a first-request
retry on wake (the original resumed explicitly; Substrate wakes on the first request).

## Prereqs (Linux box)

- `~/.venv-ray-substrate` with the env client plus `torch` (CPU wheel), `trl==0.21.0`,
  `transformers<5`, `datasets`, `accelerate`.
- A template whose guest image has `python3`. The stock `ate-env-guest` image is built on
  `bash:latest`; build one on `python:3.11-slim` by overriding the base in `.ko.yaml` for one
  build, then `ate-env manifest template --template grpo-template --guest-image <digest>`.
- `kubectl -n ate-env port-forward svc/ate-env-api 7777:7777` to the target cluster.

## Run

```bash
source ~/.venv-ray-substrate/bin/activate
cd ~/env/examples/grpo
# 1. wiring, no model (~30 s): create fleet, eval good+bad program, suspend, wake, eval, delete
SMOKE=1 REWARD_SANDBOXES=2 python grpo_substrate_train.py
# 2. short training run, no suspension (baseline)
MAX_STEPS=10 MAX_COMPLETION=128 SUSPEND_MODE=off  RUN_ID=grpo-off  python grpo_substrate_train.py
# 3. same with the fleet suspended after every step (the RL park/wake pattern)
MAX_STEPS=10 MAX_COMPLETION=128 SUSPEND_MODE=per_step EPISODE_LEDGER=1 RUN_ID=grpo-park python grpo_substrate_train.py
python report.py /tmp/grpo-substrate-grpo-off.jsonl
python report.py /tmp/grpo-substrate-grpo-park.jsonl
# 4. per_step with a realistic working set per actor (puts the real loop on the T1 size curve)
STATE_MB=512  MAX_STEPS=10 MAX_COMPLETION=128 SUSPEND_MODE=per_step EPISODE_LEDGER=1 RUN_ID=grpo-park-512m python grpo_substrate_train.py
STATE_MB=1024 MAX_STEPS=10 MAX_COMPLETION=128 SUSPEND_MODE=per_step EPISODE_LEDGER=1 RUN_ID=grpo-park-1g   python grpo_substrate_train.py
```

First run downloads the model (~1 GB) and MBPP from Hugging Face.

## What was tested, what came out, and why it matters

### The test

The same training loop was run on two clusters a few days apart. Nothing in the trainer changed between
them: Qwen2.5-0.5B, GRPO, the MBPP problems, eight completions per step, ten steps, the reward computed by
running each completion's asserts inside its own actor. The only knob that changed between runs within a
cluster was the working set: how much memory each actor holds when it is suspended (none, 512 MiB, or
1 GiB of random bytes, which cannot be compressed away). The fleet was suspended after every step and woken
by the next step's first command, which is what a trainer does between sampling rounds.

| | Cluster A | m2-density-a |
|---|---|---|
| When | Oct 12-13, 2026 | Oct 16, 2026 |
| Nodes | 4 x c3-standard-4 (4 vCPU, 16 GiB) | 139 x n2-standard-8 (8 vCPU, 32 GiB) |
| Region | us-west1 | us-central1 |
| Substrate | 2e3814a0 | ac5c5ee0 (upstream head on Oct 14) |
| Sandbox | gVisor, snapshots in a bucket in the same region, pd-balanced boot disks | same |
| Actors per node with 8 reward actors | two | one |

### What came out

Every run on both clusters: all 80 evaluations correct, no state lost across any suspend and wake, and
the same reward curve run to run. The loop is real and it is repeatable.

Parking cost per step, which is the time to suspend the whole fleet plus the time to wake it:

| Fleet and working set | Cluster A | m2-density-a |
|---|---|---|
| 8 actors, nothing in memory | 1.4 s | 3.0 s |
| 8 actors, 512 MiB each | 30 s | 12 s |
| 8 actors, 1 GiB each | 100 s | 21 s |
| 64 actors, 512 MiB each | not run: 64 x 512 MiB does not fit four 16 GiB nodes | 17 s |

On m2, one actor with 512 MiB suspends in about 4 s and wakes in about 3 s; with 1 GiB, about 8 s and 7 s.
The eight evaluations of a step take a quarter of a second everywhere.

To separate the effect of placement from the effect of the newer build and bigger machines, the m2 worker
pool was then shrunk to four workers and the two heavy runs repeated with two actors per node, as on
cluster A:

| 8 actors, working set | Cluster A (2 per node, old build) | m2, 2 per node | m2, 1 per node |
|---|---|---|---|
| 512 MiB | 30 s | 18 s | 12 s |
| 1 GiB | 100 s | 32 s | 21 s |

Putting two actors on a node costs about half again as much parking as one per node, and pushes more
wakes past the router's ten-second parking budget (28 retries against 4 at 1 GiB). The rest of the gap to
cluster A comes from the newer build and the larger machines.

### Why it matters

- **Spreading the fleet is worth as much as a release.** Parking a real working set got three to five
  times cheaper between the two clusters. About a third of that is placement: two actors sharing a node's
  disk take half again as long to park as one per node, because suspend and wake are bounded by how fast
  one node can write and read its disk. The rest is the newer build on bigger machines. Placement is the
  part an operator controls today.
- **A bigger fleet costs little more.** Sixty-four actors park in 17 s against 12 s for eight, because each
  node does its share in parallel. The fleet-level time is set by the slowest node, not by the count.
- **Image locality is the floor.** The one case that got slower on the big cluster is the near-empty guest:
  its wake went from 0.7 s to 2.3 s because on 139 nodes the woken actor usually lands on a node that does
  not hold the image yet, so even a small image has to be pulled before the actor can answer. Every wake
  pays this unless the scheduler puts the actor where its image already is.
- **On a GPU trainer these seconds are the step.** Here the trainer step is about 55 s of CPU arithmetic, so
  the sandbox side is under one percent of it. A GPU trainer finishes the same step in a second or two, and
  then 12 to 21 s of parking per step is most of the step. That is why the number to publish is parking
  time per step, not its share of this slow loop.
- **For comparison**, the same trainer on agent-sandbox pod snapshots in September 2026 parked at about 7 s
  per step regardless of working set, and with the reward fleet split across two Substrate clusters (four
  actors each, Oct 14) parking cost about 0.9 s per step while the trainer saw a single fleet.

## What to record

- `report.py` output for the off and per_step runs: eval p50, suspend p50, wake (resume) p50,
  retries>0 count, heals, evals ok.
- Step wall difference between the two runs = cost of parking per step (upstream measured a
  constant ~7 s/step on agent-sandbox pod snapshots).
- Any `ledger_mismatch` line: state did not survive a suspend/wake cycle.
- Reward trend first vs. last steps: proves the gradient path is real, not that Substrate is
  fast. With 0.5B on CPU expect noisy 0.1 to 0.4.

## Knobs

Same names as the original (`MODEL, MAX_STEPS, REWARD_SANDBOXES, NUM_GENERATIONS,
MAX_COMPLETION, SUSPEND_MODE, DATASET_SLICE, ROLLOUT_LEN, EPISODE_LEDGER, BF16`) plus
`ATE_ENV_ENDPOINT, ATE_TEMPLATE, ATE_ATESPACE, RUN_ID, EXEC_TIMEOUT, WAKE_RETRIES, WAKE_TIMEOUT,
OUT, SMOKE`. Actors are named `RUN_ID-<n>`; a crashed run is swept with
`kubectl-ate get actors -a ate-env | awk '/^RUN_ID-/{print $1}' | xargs -n1 kubectl-ate delete actor -a ate-env`
(suspend first for any still RUNNING).

## Two clusters, one training loop (MC-1 on the harness side)

`ATE_ENV_ENDPOINTS` takes a comma-separated `NAME=HOST:PORT` list; the reward fleet is created
round-robin across those clusters and every later call is routed to the cluster that owns the actor.
Each cluster needs the template (`grpo-template`, python guest) and its own port-forward:

```bash
kubectl --context <ctx-A> -n ate-env port-forward svc/ate-env-api 7777:7777 >/dev/null 2>&1 &
kubectl --context <ctx-B> -n ate-env port-forward svc/ate-env-api 7778:7777 >/dev/null 2>&1 &
export ATE_ENV_ENDPOINTS="A=localhost:7777,B=localhost:7778"
SMOKE=1 REWARD_SANDBOXES=4 OUT=/tmp/grpo-mc-smoke.jsonl python grpo_substrate_train.py && python report.py /tmp/grpo-mc-smoke.jsonl
MAX_STEPS=10 MAX_COMPLETION=128 SUSPEND_MODE=per_step EPISODE_LEDGER=1 RUN_ID=grpo-mc python grpo_substrate_train.py
python report.py /tmp/grpo-substrate-grpo-mc.jsonl   # per-cluster boot/eval/suspend/resume rows at the end
```

Every JSONL record carries `cluster`, so the per-cluster table in `report.py` shows whether one
cluster is slower to park or wake than the other. Sweep leftovers fleet-wide with
`substrate/hack/scratch/ate-fleet -sweep <RUN_ID>`.
