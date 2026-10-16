# GRPO on Substrate: a real RL loop with actors as the reward executor

Port of the agent-sandbox stress harness trainer (`vicentefb/agent-sandbox#7`,
`examples/stress/snapshot_ray/harness/grpo_sandbox_train.py`) to Substrate's env client.
TRL's GRPOTrainer trains Qwen2.5-0.5B on MBPP; each completion is executed inside a gVisor
actor and the reward is the fraction of the task's asserts that pass. CPU only.

What changed vs. the original: only the seven sandbox calls (see the module docstring).
What is new: a `SMOKE=1` mode that checks the wiring without loading a model, a JSONL record
of every create / boot / eval / suspend / wake / heal with wall time, and a first-request
retry on wake (the original resumed explicitly; Substrate wakes on the first request).

## Prereqs

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

## Measured

Same trainer, same tasks, ten steps, eight reward actors unless stated; every run 80 of 80 evals, zero state
misses, identical reward trajectories. Parking per step is the fleet suspend plus the fleet wake.

| Fleet and working set | Cluster A, 4 x c3-standard-4, Substrate 2e3814a0 (Oct 12-13) | m2-density-a, 139 x n2-standard-8, Substrate ac5c5ee0 (Oct 16) | m2 per actor, suspend / wake p50 |
|---|---|---|---|
| 8 actors, idle guest | 1.4 s | 3.0 s | 0.5 s / 2.3 s |
| 8 actors, 512 MiB random | 30 s | 12 s | 4.3 s / 3.2 s |
| 8 actors, 1 GiB random | 100 s | 21 s | 8.4 s / 6.8 s |
| 64 actors, 512 MiB random | not run: 64 x 512 MiB does not fit four 16 GiB nodes | 17 s | 3.3 s / 2.6 s |

On cluster A two actors shared each node's disk; on m2 every actor had its own node. Suspend and wake are
bounded by per-node disk throughput, so spreading the fleet cut the parking cost of a real working set by
2.5-5x with no change to Substrate, and 64 actors parked in 1.4x the time of 8. The idle guest is the one
case that got slower, all of it in the wake: on a large pool the woken actor usually lands on a node that
does not hold the image yet. The eight evals of a step take about 0.25 s in every configuration; on a CPU
trainer whose step is about 55 s the sandbox side is under one percent, and on a GPU trainer the parking
time per step above is the number that matters. The same trainer on agent-sandbox pod snapshots
(September 2026) parked at about 7 s per step regardless of working set.

With the reward fleet split across two clusters (four actors each, round-robin placement, every later call
routed to the owning cluster; Oct 14), parking the whole fleet cost about 0.9 s per step and the trainer
saw one fleet.

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
