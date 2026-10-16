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


"""Summarize a grpo_substrate_train.py OUT file: per-event p50/p95/max and per-step totals.
usage: python report.py /tmp/grpo-substrate-<RUN_ID>.jsonl
"""
import json
import statistics
import sys


def pct(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(q * (len(xs) - 1))))]


recs = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
by = {}
for r in recs:
    by.setdefault(r["ev"], []).append(r)

print(f"{'event':18} {'n':>5} {'p50 s':>8} {'p95 s':>8} {'max s':>8}  retries>0")
for ev in ["boot", "ballast", "create", "eval", "suspend", "resume", "heal", "eval_error", "suspend_failed", "resume_failed", "ledger_mismatch"]:
    rs = by.get(ev, [])
    if not rs:
        continue
    walls = [r["wall"] for r in rs if "wall" in r]
    retried = sum(1 for r in rs if r.get("retries", 0) > 0)
    if walls:
        print(f"{ev:18} {len(rs):5d} {pct(walls, .5):8.2f} {pct(walls, .95):8.2f} {max(walls):8.2f}  {retried}")
    else:
        print(f"{ev:18} {len(rs):5d}")

# Per-cluster breakdown when the run spanned several (ATE_ENV_ENDPOINTS).
clusters = sorted({r.get("cluster") for r in recs if r.get("cluster")})
if len(clusters) > 1:
    print()
    print(f"{'cluster':10} {'event':10} {'n':>5} {'p50 s':>8} {'p95 s':>8} {'max s':>8}")
    for c in clusters:
        for ev in ["boot", "eval", "suspend", "resume"]:
            walls = [r["wall"] for r in by.get(ev, []) if r.get("cluster") == c and "wall" in r]
            if walls:
                print(f"{c:10} {ev:10} {len(walls):5d} {pct(walls, .5):8.2f} {pct(walls, .95):8.2f} {max(walls):8.2f}")

steps = by.get("step", [])
if steps:
    print()
    print(f"steps: {len(steps)}  mean reward first/last: "
          f"{statistics.mean(s['reward_mean'] for s in steps[:3]):.2f} / {statistics.mean(s['reward_mean'] for s in steps[-3:]):.2f}")
    for k in ["eval_wall", "suspend_wall", "resume_wall", "recycle_wall"]:
        v = [s[k] for s in steps if k in s]
        if v:
            print(f"  {k:13} total {sum(v):8.1f} s   per step p50 {pct(v, .5):6.2f}  p95 {pct(v, .95):6.2f}")
    evals = by.get("eval", [])
    if evals:
        print(f"  evals ok: {sum(1 for e in evals if e['ok'])}/{len(evals)}")
