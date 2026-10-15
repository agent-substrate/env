# Fast Substrate RL benchmark (SWE-bench Verified)

A loader that drives real SWE-bench task images through the ate-env API the
way a simulated RL job does, and a report over what every phase cost. It is
the "Fast" rung of the RL benchmark ladder: regular runs that catch
regressions in the sandbox SDK, Substrate or the controller, on a workload
that is the real thing rather than a Python image printing READY.

Code: `internal/benchmarking/swebench` (library, tested against an in-memory
backend) and `cmd/benchmarking/swebench-loader` (binary). The layout follows
substrate's `internal/benchmarking/boomer` so a user class can be lifted into
that harness later; Go does not allow importing another module's `internal`
packages, so nothing is imported from it.

## Workloads

Both are parameters of one binary. Images, fan-out, width, steps and the
trajectory are all flags, so the smoke test and the 500-wide run differ only
in numbers.

| Shape | Flags | What happens |
|---|---|---|
| eval (SWE-bench Verified evaluation) | `-mode eval -images 500 -tasks-per-image 6 -width 500` | 3,000 tasks, 500 in flight. Each: create from the image, replay the trajectory, record the reward, delete. |
| steps (training loop) | `-mode steps -images 500 -rollouts 2 -steps 8 -park suspend` | 1,000 environments created once. Each step replays the trajectory on all of them, waits for the last (the barrier), then parks the fleet. `-park` is `suspend`, `delete` or `none`. |

The trajectory is YAML (`internal/benchmarking/swebench/trajectories/`,
or `-trajectory FILE`). The built-in `swebench-gold` does what a SWE agent
does on a task, with the gold patch as the model's edit: explore, read, write
the patch, `git apply`, write the harness eval script, run it. The reward is
the eval script's exit code, so a healthy run rewards every task and a drop
in reward rate is a correctness regression. Steps carry think times, the
simulated policy latency between commands.

## What is measured

One JSONL event per operation, wall time as the loader sees it (network and
API server included, which is what a trainer sees):

| Event | Meaning |
|---|---|
| `create` | CreateEnvironment call |
| `ttfi` | create start to the first command's return (time to first instruction) |
| `wake` | first command after a suspend park, including the wake it triggers |
| `step` (by name) | each trajectory step, with exit code; the reward step carries the reward and an output tail |
| `task` | one replay end to end, ok and reward |
| `suspend`, `delete` | the calls |
| `step_barrier` | steps mode: first task start to last task end in a training step |
| `park` | steps mode: parking the whole fleet |
| `run` | the whole run |

`swebench-loader report RUN.jsonl -json summary.json` prints count, failed,
p50, p95 and max per event, a per-step table (tasks, barrier, park, wake
p50/p95) and the most frequent errors. The JSON is the artifact to diff
between runs.

## Prepare once per project

1. Mirror the task images into a registry the nodes can pull from (the shared
   mirrors deny the node identity). One tag per line in `tags.txt`:

   ```bash
   benchmarking/swebench/mirror-images.sh \
     us-docker.pkg.dev/gke-ai-eco-dev/swebench-mirror/swebench-verified \
     us-central1-docker.pkg.dev/PROJECT/swebench tags.txt 8
   ```

2. Build the manifest: gold patch, test patch, harness eval script and the
   digest-pinned image per instance.

   ```bash
   pip install datasets 'swebench>=5'
   benchmarking/swebench/make-dataset.py \
     --registry us-docker.pkg.dev/PROJECT/swebench-mirror/swebench-verified \
     --out swebench-verified.jsonl
   ```

   `--image-ref` sets how an instance maps to an image reference (default:
   one tag per instance, `sweb.eval.x86_64.<instance_id>`, in one
   repository); the script lists instances whose image it cannot resolve.

3. Install ate-env with a base template (`ate-env manifest template`). Per
   image, the API derives one template from it on first use and bakes the
   golden snapshot then; the first create of each image pays that.

## Run

From a machine with a port-forward (fine for the smoke test and the step
loop):

```bash
kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &
go run ./cmd/benchmarking/swebench-loader run -dataset swebench-verified.jsonl \
  -images 5 -tasks-per-image 1 -width 5 -run-id smoke
go run ./cmd/benchmarking/swebench-loader report smoke.jsonl
```

`-dry-run` loads and renders everything and prints the plan without touching
the API. `-keep` leaves the fleet in place for inspection.

For the 500-wide shapes run inside the cluster so the loader sits next to the
API: `manifests/loader-job.yaml` (fill the `${...}` values with `envsubst`;
the image comes from `ko build ./cmd/benchmarking/swebench-loader`).

## Record with every run

Substrate and env commits, cluster shape (machine type, node count, sandbox
class), bucket region, the dataset manifest's digest, and the loader flags.
The report alone is not reproducible without them.
