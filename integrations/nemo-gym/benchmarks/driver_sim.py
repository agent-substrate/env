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

"""Substrate rollout-driver simulator: start-time / time-to-first-command bench.

Mirrors the agent-sandbox `asrl-driver` bench convention (per task: create the
sandbox, deliver the environment, run a first `exec` probe — probe defaults to
`true`, isolating start latency). Here each task is a Substrate actor driven
through the substrate NeMo Gym provider (create -> wait ready -> exec -> delete).

Phases measured per task:
  * ready_s     — create actor + wait until it serves commands (env delivery)
  * first_cmd_s — the first exec (probe) round-trip
  * ttfe_s      — ready_s + first_cmd_s (time to first execution)

Templates must be pre-provisioned (image + ate-env-guest, one per image). See
hack/bake-task-image.sh. Input file lists one substrate template name per line
(optionally `template<TAB>display-image`); blank lines and #comments ignored.

Usage:
  kubectl port-forward -n ate-env svc/ate-env-api 7777:7777 &
  python driver_sim.py --templates-file r2e_templates.txt \
      --rollouts 2 --concurrency 5 --probe true --out results.json
"""

from __future__ import annotations

import argparse
import asyncio
import json
import statistics
import time

from nemo_gym_substrate._compat import SandboxSpec
from nemo_gym_substrate.provider import SubstrateSandboxProvider


def _pct(values: list[float], p: float) -> float | None:
    if not values:
        return None
    if len(values) == 1:
        return round(values[0], 3)
    q = statistics.quantiles(values, n=100, method="inclusive")
    return round(q[min(p, len(q)) - 1], 3)


def _summary(name: str, values: list[float]) -> dict:
    if not values:
        return {"phase": name, "n": 0}
    return {
        "phase": name, "n": len(values),
        "p50": _pct(values, 50), "p90": _pct(values, 90),
        "max": round(max(values), 3), "mean": round(statistics.fmean(values), 3),
    }


async def _task(provider, template, namespace, rollout, probe, ready_timeout, sem, out):
    async with sem:
        opts = {"template": template}
        if namespace:
            opts["namespace"] = namespace
        rec = {"template": template, "rollout": rollout, "ok": False}
        handle = None
        try:
            t0 = time.monotonic()
            handle = await provider.create(
                SandboxSpec(provider_options=opts, ready_timeout_s=ready_timeout)
            )
            rec["ready_s"] = time.monotonic() - t0
            t1 = time.monotonic()
            res = await provider.exec(handle, probe)
            rec["first_cmd_s"] = time.monotonic() - t1
            rec["ttfe_s"] = rec["ready_s"] + rec["first_cmd_s"]
            rec["exit_code"] = res.return_code
            rec["ok"] = res.return_code == 0
        except Exception as exc:  # noqa: BLE001 - record, keep the sweep going
            rec["error"] = f"{type(exc).__name__}: {str(exc)[:200]}"
        finally:
            if handle is not None:
                try:
                    await provider.close(handle)
                except Exception:  # noqa: BLE001
                    pass
        status = "ok" if rec["ok"] else f"ERR {rec.get('error', rec.get('exit_code'))}"
        print(f"  [{template.split('/')[-1][:48]:48} r{rollout}] "
              f"ttfe={rec.get('ttfe_s', float('nan')):.2f}s "
              f"(ready={rec.get('ready_s', float('nan')):.2f} "
              f"cmd={rec.get('first_cmd_s', float('nan')):.2f}) {status}", flush=True)
        out.append(rec)


def _load_templates(path: str, limit: int) -> list[str]:
    templates = []
    for line in open(path):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        templates.append(line.split("\t")[0].split()[0])
    return templates[:limit] if limit else templates


async def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--api-url", default="http://127.0.0.1:7777")
    ap.add_argument("--templates-file", required=True)
    ap.add_argument("--rollouts", type=int, default=1, help="tasks per template")
    ap.add_argument("--concurrency", type=int, default=5)
    ap.add_argument("--probe", default="true", help="first command (default no-op)")
    ap.add_argument("--problems", type=int, default=0, help="limit templates (0=all)")
    ap.add_argument("--ready-timeout-s", type=float, default=300.0)
    ap.add_argument("--namespace", default=None)
    ap.add_argument("--out", default=None)
    a = ap.parse_args()

    templates = _load_templates(a.templates_file, a.problems)
    tasks_plan = [(t, g) for t in templates for g in range(a.rollouts)]
    print(f"driver: {len(templates)} templates x {a.rollouts} = {len(tasks_plan)} tasks; "
          f"concurrency={a.concurrency} probe={a.probe!r}", flush=True)

    provider = SubstrateSandboxProvider({"connection": {"api_url": a.api_url}})
    sem = asyncio.Semaphore(a.concurrency)
    out: list[dict] = []

    wall0 = time.monotonic()
    await asyncio.gather(*(
        _task(provider, t, a.namespace, g, a.probe, a.ready_timeout_s, sem, out)
        for t, g in tasks_plan
    ))
    wall = time.monotonic() - wall0
    await provider.aclose()

    ok = [r for r in out if r["ok"]]
    ready = [r["ready_s"] for r in ok]
    cmd = [r["first_cmd_s"] for r in ok]
    ttfe = [r["ttfe_s"] for r in ok]
    summary = {
        "tasks": len(out), "ok": len(ok), "err": len(out) - len(ok),
        "wall_s": round(wall, 1),
        "throughput_tasks_per_s": round(len(out) / wall, 3) if wall else None,
        "phases": [_summary("ready", ready), _summary("first_cmd", cmd), _summary("ttfe", ttfe)],
    }
    print("\n=== RESULT ===")
    print(json.dumps(summary, indent=2))
    if a.out:
        json.dump({"summary": summary, "tasks": out}, open(a.out, "w"), indent=2)
        print(f"wrote {a.out}")


if __name__ == "__main__":
    asyncio.run(main())
