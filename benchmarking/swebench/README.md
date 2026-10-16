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
| eval (SWE-bench Verified evaluation) | `-mode eval -images 500 -tasks-per-image 6 -width 500` | 500 in flight. Each task: create from the image, replay the trajectory, record the reward, delete. With the offline exclusion list the 500 become 445 images and 2,670 tasks; the loader prints the reduced count. |
| steps (training loop) | `-mode steps -images 500 -rollouts 2 -steps 8 -park suspend` | 1,000 environments created once. Each step replays the trajectory on all of them, waits for the last (the barrier), then parks the fleet. `-park` is `suspend`, `delete` or `none`. |

The trajectory is YAML (`internal/benchmarking/swebench/trajectories/`,
or `-trajectory FILE`). The built-in `swebench-gold` does what a SWE agent
does on a task, with the gold patch as the model's edit: explore, read, write
the patch, `git apply`, write the harness eval script, run it. The reward is
the SWE-bench verdict: `swebench-grade.py` (written into the sandbox by the
trajectory) parses the log with the repo's harness parser and checks the
instance's FAIL_TO_PASS and PASS_TO_PASS tests, the same rules the harness
applies. Neither the script's nor the test command's exit code is the
verdict: unrelated tests in the same files fail routinely. A healthy run
rewards every task and a drop in reward rate is a correctness regression.
`python3 -m pytest benchmarking/swebench/tests` covers the grader. Steps carry think times, the
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

1. Put the task images in a registry the nodes can pull from. If the nodes
   can read your mirror directly, skip this step. Otherwise copy them by
   digest with crane; `tags.txt` has one tag per line, for example every
   `sweb.eval.x86_64.*` tag of the source repository without the `-zran`
   variants:

   ```bash
   SRC=SOURCE_REGISTRY/swebench-verified
   DST=us-central1-docker.pkg.dev/PROJECT/swebench/swebench-verified
   crane ls $SRC | grep -v -- '-zran$' > tags.txt
   benchmarking/swebench/mirror-images.sh $SRC $DST tags.txt 8
   ```

2. Build the manifest: gold patch, test patch, harness eval script, log
   parser and the digest-pinned image per instance. `--registry` is the
   repository the images live in, the same `$DST` as above (or the source
   repository when the nodes can read it).

   ```bash
   pip install datasets 'swebench>=5'
   benchmarking/swebench/make-dataset.py --registry $DST \
     --revision <dataset commit> --out swebench-verified.jsonl
   ```

   `--image-ref` sets how an instance maps to an image reference (default:
   one tag per instance, `sweb.eval.x86_64.<instance_id>`, in one
   repository); the script lists instances whose image it cannot resolve and
   writes the dataset revision and swebench version next to the manifest.

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
API and spreads over its replicas (`-connections`, default 4):

```bash
KO_DOCKER_REPO=gcr.io/PROJECT/repo benchmarking/swebench/run-in-cluster.sh swe-eval500x6 swebench-verified.jsonl \
  -mode eval -images 500 -tasks-per-image 6 -width 500 -connections 8
```

The script bakes the dataset and the exclusion list into the loader image,
runs a Job next to ate-env-api, follows the full loader log (step failures
included), copies the events file back to the current directory, prints the
report and deletes the Job.

## Network and the offline exclusion list

Actors have no network until an egress policy is created for them, which the
env SDK cannot do yet, and the SWE-bench harness assumes network (matplotlib's
eval scripts pip-install, requests and django have tests that open sockets).
`offline-exclusions.txt` lists the Verified instances that do not resolve
without egress, with the grader's verdict as the reason; pass it with
`-exclude-file` and every remaining instance should resolve. Regenerate it
from a full pass when the dataset, the images or the sandbox network change,
and review the diff: a new entry is either a regression or a new
network-dependent test, never silently accepted. Egress at scale is a
benchmark of its own.

## Two things the first runs showed

- Commands run as start, poll, read rather than through the SDK's streaming
  shell: the router's route timeout (five minutes by default) cuts a single
  streamed request, and SWE-bench test runs regularly exceed it.
- On a large pool a new actor rarely lands on a node that already has its
  image, so time to first instruction is the cold pull plus the golden bake
  (about 20 s) for nearly every task. Report cold and warm passes separately.

## Record with every run

Substrate and env commits, cluster shape (machine type, node count, sandbox
class), bucket region, the dataset manifest's digest, and the loader flags.
The report alone is not reproducible without them.
