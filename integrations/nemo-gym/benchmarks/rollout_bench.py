# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Mock NeMo-RL rollout benchmark for the substrate sandbox provider.

Shapes the load like a NeMo Gym rollout batch: N parallel rollouts, each
create -> seed task file -> T agent turns (exec + simulated model "think"
time) -> artifact download -> close. The LLM is mocked with asyncio.sleep;
the sandboxes are real Substrate actors.

Usage:
  kubectl port-forward -n ate-env svc/ate-env-api 7777:7777 &
  python benchmarks/rollout_bench.py --rollouts 5 --turns 3 --think-s 2
"""

from __future__ import annotations

import argparse
import asyncio
import statistics
import time

from nemo_gym_substrate._compat import SandboxSpec
from nemo_gym_substrate.provider import SubstrateSandboxProvider

# Pure-sh turn work: append the turn marker to the episode log, emit a
# checksum of the log so far. Runs in any guest with a POSIX shell.
TURN_CMD = 'echo "turn {turn} $(date +%s%N)" >> /task/episode.log && wc -c < /task/episode.log'


async def rollout(
    provider: SubstrateSandboxProvider,
    index: int,
    turns: int,
    think_s: float,
    template: str | None,
    metrics: dict[str, list[float]],
    sem: asyncio.Semaphore,
) -> None:
    async with sem:
        opts = {"template": template} if template else {}
        t0 = time.monotonic()
        handle = await provider.create(
            SandboxSpec(
                files={"/task/episode.log": f"rollout {index}\n"},
                workdir="/task",
                provider_options=opts,
                ready_timeout_s=180,
            )
        )
        metrics["create_s"].append(time.monotonic() - t0)
        try:
            for turn in range(turns):
                t1 = time.monotonic()
                result = await provider.exec(handle, TURN_CMD.format(turn=turn))
                metrics["exec_s"].append(time.monotonic() - t1)
                if result.return_code != 0:
                    raise RuntimeError(
                        f"rollout {index} turn {turn} failed rc={result.return_code}: "
                        f"{result.stderr}"
                    )
                await asyncio.sleep(think_s)  # mock model inference / eval
        finally:
            t2 = time.monotonic()
            await provider.close(handle)
            metrics["close_s"].append(time.monotonic() - t2)


def summarize(name: str, values: list[float]) -> str:
    if not values:
        return f"{name:>9}: n=0"
    q = statistics.quantiles(values, n=20) if len(values) >= 2 else [values[0]] * 19
    return (
        f"{name:>9}: n={len(values):<4} p50={statistics.median(values):6.2f}s "
        f"p95={q[18]:6.2f}s max={max(values):6.2f}s"
    )


async def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--api-url", default="http://127.0.0.1:7777")
    ap.add_argument("--rollouts", type=int, default=5)
    ap.add_argument("--turns", type=int, default=3)
    ap.add_argument("--think-s", type=float, default=2.0)
    ap.add_argument("--max-concurrency", type=int, default=5,
                    help="parallel rollouts (bound by warm workers on small clusters)")
    ap.add_argument("--template", default=None)
    args = ap.parse_args()

    provider = SubstrateSandboxProvider({"connection": {"api_url": args.api_url}})
    metrics: dict[str, list[float]] = {"create_s": [], "exec_s": [], "close_s": []}
    sem = asyncio.Semaphore(args.max_concurrency)

    wall0 = time.monotonic()
    results = await asyncio.gather(
        *(
            rollout(provider, i, args.turns, args.think_s, args.template, metrics, sem)
            for i in range(args.rollouts)
        ),
        return_exceptions=True,
    )
    wall = time.monotonic() - wall0
    await provider.aclose()

    failures = [r for r in results if isinstance(r, BaseException)]
    execs = len(metrics["exec_s"])
    print(f"\nrollouts={args.rollouts} turns={args.turns} think={args.think_s}s "
          f"concurrency={args.max_concurrency}")
    print(summarize("create", metrics["create_s"]))
    print(summarize("exec", metrics["exec_s"]))
    print(summarize("close", metrics["close_s"]))
    ideal = args.turns * args.think_s  # pure think time per rollout
    print(f"{'wall':>9}: {wall:6.2f}s   execs/s={execs / wall:5.2f}   "
          f"think-only floor per rollout={ideal:.1f}s")
    if failures:
        print(f"FAILURES: {len(failures)}")
        for f in failures[:3]:
            print("  -", type(f).__name__, str(f)[:200])
        raise SystemExit(1)


if __name__ == "__main__":
    asyncio.run(main())
