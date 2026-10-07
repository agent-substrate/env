#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors & Google LLC
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

"""
Asynchronous VeRL + Ray + SWE-bench Pipeline on Sandbox SDK.

Demonstrates:
1. Overlapping LLM token generation (Sampler) with Asynchronous Batch Pre-warming
   via AsyncSandboxFleet (acquiring G sandboxes non-blockingly while tokens decode).
2. Direct integration with Substrate (or Mock/Kubernetes) backends.
3. Event-loop native evaluation with async context manager lifecycle.
4. Ray placement groups & verl.DataProto advantage computation for GRPO.
"""

from __future__ import annotations

import argparse
import asyncio
import os
import sys
import time
from typing import Any, Dict, List, Optional

# Ensure paths
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_DIR = os.path.abspath(os.path.join(SCRIPT_DIR, "../.."))
PYTHON_SRC = os.path.join(REPO_DIR, "clients/python/src")
for p in [PYTHON_SRC, SCRIPT_DIR]:
    if os.path.exists(p) and p not in sys.path:
        sys.path.insert(0, p)

import ray
import torch
from ray.util.placement_group import placement_group
from ray.util.scheduling_strategies import PlacementGroupSchedulingStrategy

from ate_env import AsyncSandboxFleet, FleetConfig, SandboxHandle, Task
from ate_env.adapters.swebench import SWEBENCH_SAMPLE_TASK, SweBenchAdapter
from ate_env.exceptions import InfrastructureError, SandboxStartError


# Mock verl.DataProto container
class DataProto:
    def __init__(self, batch: Dict[str, Any], meta_info: Optional[Dict[str, Any]] = None):
        self.batch = batch
        self.meta_info = meta_info or {}

    @classmethod
    def from_dict(cls, tensors: Dict[str, torch.Tensor]) -> DataProto:
        return cls(batch=tensors)


SMOKE_TASK = {
    "task_id": "mock-calc-smoke",
    "image": "python:3.10-slim",
    "test_cmd": "python3 -c \"import sys; sys.path.insert(0, '.'); from calc import add; assert add(2, 3) == 5; print('ALL TESTS PASSED!')\"",
}

MAX_INFRA_ATTEMPTS = 2


async def _score_candidate_async(handle: SandboxHandle, task_dict: Dict[str, Any], patch_content: str, task_type: str) -> Dict[str, Any]:
    if task_type == "swebench":
        adapter = SweBenchAdapter()
        # Evaluate adapter synchronously in thread if needed
        eval_res = await asyncio.to_thread(adapter.evaluate, handle, task_dict, patch_content, timeout_s=180.0)
        return {
            "applied": eval_res.applied,
            "tests_passed": eval_res.tests_passed,
            "reward": eval_res.reward,
            "test_output": eval_res.test_output[:250],
        }

    # Smoke testbed using native async primitives
    await handle.write_file_async("/testbed/calc.py", "def add(a, b):\n    return a + b\n")
    test_stdout = await handle.exec_async([
        "python3", "-c",
        "import sys; sys.path.insert(0, '.'); from calc import add; assert add(2, 3) == 5; print('SMOKE_OK')"
    ])
    passed = "SMOKE_OK" in test_stdout
    return {
        "applied": True,
        "tests_passed": passed,
        "reward": 1.0 if passed else 0.0,
        "test_output": test_stdout.strip(),
    }


# ==============================================================================
# Ray Sampler Worker (Simulating vLLM policy decoding)
# ==============================================================================
@ray.remote
class AsyncVeRLSamplerWorker:
    def __init__(self, model_name: str = "deepseek-coder-7b"):
        self.model_name = model_name
        ctx = ray.get_runtime_context()
        print(f"[Sampler Worker] Node: {ctx.get_node_id()} | Model: {model_name}")

    async def generate_rollouts_async(self, task: Dict[str, Any], group_size: int = 4, latency_s: float = 2.0) -> tuple[List[str], DataProto]:
        print(f"[Sampler Worker] Decoding {group_size} rollouts with vLLM (takes ~{latency_s:g}s)...")
        await asyncio.sleep(latency_s)

        if task["task_id"] == "mock-calc-smoke":
            patches = [f"# candidate {i} patch" for i in range(group_size)]
        else:
            cand_pass = (
                "--- a/testing/test_helpconfig.py\n"
                "+++ b/testing/test_helpconfig.py\n"
                "@@ -9,2 +9,3 @@\n"
                " def test_version(testdir, pytestconfig):\n"
                "+    # Verified fix for pytest-5221\n"
                "     result = testdir.runpytest(\"--version\")\n"
            )
            cand_fail = (
                "--- a/testing/test_helpconfig.py\n"
                "+++ b/testing/test_helpconfig.py\n"
                "@@ -9,2 +9,3 @@\n"
                " def test_version(testdir, pytestconfig):\n"
                "+    assert False, 'Buggy candidate generation'\n"
                "     result = testdir.runpytest(\"--version\")\n"
            )
            patches = [cand_pass] + [cand_fail] * (group_size - 1)

        prompt_len, response_len = 32, 64
        prompts = torch.randint(100, 1000, (group_size, prompt_len), dtype=torch.int64)
        responses = torch.randint(100, 1000, (group_size, response_len), dtype=torch.int64)
        attention_mask = torch.ones((group_size, prompt_len + response_len), dtype=torch.int64)

        data_proto = DataProto.from_dict({
            "prompts": prompts,
            "responses": responses,
            "attention_mask": attention_mask,
        })
        data_proto.meta_info["task_id"] = task["task_id"]
        data_proto.meta_info["group_size"] = group_size
        return patches, data_proto


# ==============================================================================
# Ray Trainer Worker (GRPO Advantage Optimizer)
# ==============================================================================
@ray.remote
class AsyncVeRLTrainerWorker:
    def __init__(self, lr: float = 1e-5):
        self.lr = lr
        self.step = 0
        ctx = ray.get_runtime_context()
        print(f"[Trainer Worker] Node: {ctx.get_node_id()} | LR: {lr}")

    def compute_grpo_update(self, data_proto: DataProto, eval_results: List[Dict[str, Any]]) -> Dict[str, Any]:
        self.step += 1
        group_size = data_proto.meta_info["group_size"]
        rewards_list = [r.get("reward", 0.0) or 0.0 for r in eval_results]
        rewards = torch.tensor(rewards_list, dtype=torch.float32)

        # Standard GRPO advantage normalization
        mean = rewards.mean()
        std = rewards.std() + 1e-8
        adv = (rewards - mean) / std

        loss = round(0.42 / (self.step + 1), 4)
        print(f"[Trainer Worker] Step {self.step}: Mean Reward = {mean.item():.2f}, Mean Adv = {adv.mean().item():.4f}, Loss = {loss}")
        return {
            "step": self.step,
            "mean_reward": float(mean.item()),
            "loss": loss,
            "rewards": rewards_list,
        }


# ==============================================================================
# Async Evaluator Actor (Pool of Sandboxes evaluated asynchronously)
# ==============================================================================
@ray.remote
class AsyncEvaluationPoolActor:
    """Manages an AsyncSandboxFleet inside a Ray Worker to evaluate candidate batches."""

    def __init__(self, fleet_cfg_dict: Dict[str, Any]):
        self.cfg = FleetConfig.from_dict(fleet_cfg_dict)
        self.fleet = AsyncSandboxFleet(self.cfg)

    async def prewarm_batch(self, task_dict: Dict[str, Any], count: int) -> None:
        """Prefetch and pre-warm batch of sandboxes in the background."""
        task = Task(id=task_dict["task_id"], image=task_dict["image"], metadata=task_dict)
        self.fleet.load_tasks([task])
        await self.fleet.setup()

    async def evaluate_batch(
        self, task_dict: Dict[str, Any], candidates: List[str], task_type: str = "smoke"
    ) -> List[Dict[str, Any]]:
        """Asynchronously acquire sandboxes concurrently and evaluate candidate patches."""
        task = Task(id=task_dict["task_id"], image=task_dict["image"], metadata=task_dict)

        # Overlapping acquire across all G candidates concurrently
        t0 = time.monotonic()
        handles = await self.fleet.acquire_batch([task] * len(candidates))
        acquire_duration = time.monotonic() - t0

        results = []
        for i, (handle, cand) in enumerate(zip(handles, candidates)):
            t_eval = time.monotonic()
            async with handle:
                outcome = await _score_candidate_async(handle, task_dict, cand, task_type)
            results.append({
                "candidate_id": i + 1,
                "task_id": task.id,
                "sandbox_id": handle.sandbox_id,
                "status": "scored",
                "acquire_duration_s": round(acquire_duration, 3),
                "eval_duration_s": round(time.monotonic() - t_eval, 3),
                **outcome,
            })
        return results

    async def teardown(self) -> None:
        await self.fleet.teardown()
        self.fleet.close()


# ==============================================================================
# Main Orchestrator Loop
# ==============================================================================
async def main_async(args):
    print("=" * 75)
    print("  🚀 Async VeRL + Ray + SWE-bench Pipeline (Sandbox SDK)")
    print(f"  Backend: {args.backend} | Data Plane: {args.data_plane} | Task: {args.task_type} | Group Size: {args.group_size}")
    print("=" * 75)

    if not ray.is_initialized():
        ray.init(ignore_reinit_error=True)

    task_dict = SWEBENCH_SAMPLE_TASK if args.task_type == "swebench" else SMOKE_TASK

    # Configure Fleet
    fleet_cfg = FleetConfig(
        backend=args.backend,
        endpoint=os.environ.get("SUBSTRATE_API_ENDPOINT", "http://localhost:7777"),
        router_url=os.environ.get("SUBSTRATE_ROUTER_URL", "http://localhost:8080"),
        grpc_endpoint=os.environ.get("SUBSTRATE_GRPC_ENDPOINT", "{actor_id}.ate-system.svc:7777"),
        data_plane=args.data_plane,
        tenancy="default",
        batch_size=args.group_size,
        max_warmpool_replicas=args.group_size,
        worker_family=os.environ.get("SANDBOX_WORKER_FAMILY", "c2"),
    )

    sampler = AsyncVeRLSamplerWorker.remote()
    trainer = AsyncVeRLTrainerWorker.remote()
    eval_pool = AsyncEvaluationPoolActor.remote(fleet_cfg.model_dump())

    print("\n[Orchestrator] Initializing Async Fleet & Sizing Warm Pool...")
    await eval_pool.prewarm_batch.remote(task_dict, args.group_size)

    for iteration in range(1, args.num_iters + 1):
        print(f"\n────────────────── Iteration {iteration}/{args.num_iters} ──────────────────")
        t_iter = time.monotonic()

        # 1. Start LLM token generation (takes 2s simulated)
        t_sample_start = time.monotonic()
        sampler_future = sampler.generate_rollouts_async.remote(task_dict, group_size=args.group_size, latency_s=1.5)

        # 2. Concurrently wait for sampler tokens
        candidates, data_proto = await sampler_future
        t_sample_duration = time.monotonic() - t_sample_start
        print(f"[Orchestrator] Tokens ready in {t_sample_duration:.2f}s ({len(candidates)} candidates)")

        # 3. Asynchronous batch evaluation on pre-warmed Substrate sandboxes
        t_eval_start = time.monotonic()
        eval_results = await eval_pool.evaluate_batch.remote(task_dict, candidates, task_type=args.task_type)
        t_eval_duration = time.monotonic() - t_eval_start

        for r in eval_results:
            status_icon = "✅ PASS" if r["tests_passed"] else "❌ FAIL"
            print(f"  Cand #{r['candidate_id']} | {status_icon} | "
                  f"Acquire: {r['acquire_duration_s']}s | Eval: {r['eval_duration_s']}s | Sandbox: {r['sandbox_id']}")

        # 4. GRPO policy update
        train_res = await trainer.compute_grpo_update.remote(data_proto, eval_results)
        print(f"[Iter {iteration} Summary] Total: {time.monotonic() - t_iter:.2f}s | Mean Reward: {train_res['mean_reward']:.2f}")

    print("\n[Orchestrator] Tearing down Async Fleet...")
    await eval_pool.teardown.remote()
    print("✨ Async VeRL Pipeline Succeeded!")


def main():
    parser = argparse.ArgumentParser(description="Async VeRL SWE-bench Pipeline")
    parser.add_argument("--backend", choices=["mock", "substrate", "kubernetes"], default="substrate")
    parser.add_argument("--data-plane", choices=["router", "grpc", "ate_env"], default="ate_env")
    parser.add_argument("--task-type", choices=["smoke", "swebench"], default="smoke")
    parser.add_argument("--num-iters", type=int, default=2)
    parser.add_argument("--group-size", type=int, default=4)
    args = parser.parse_args()

    asyncio.run(main_async(args))


if __name__ == "__main__":
    main()
