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
```

First run downloads the model (~1 GB) and MBPP from Hugging Face.

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
