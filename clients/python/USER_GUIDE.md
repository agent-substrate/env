# Reinforcement Learning & Sandbox Fleet Guide

This guide covers how to use high-throughput **Sandbox Fleet Orchestration (`SandboxFleet` & `AsyncSandboxFleet`)** in `ate_env` with distributed Reinforcement Learning frameworks like **VeRL**, **Ray**, and **NeMo-Gym** on top of **Agent Substrate** and **GKE**.

---

## 1. Quickstart

### Synchronous Batch Execution

```python
from ate_env import FleetConfig, SandboxFleet, Task

config = FleetConfig(
    backend="substrate",
    endpoint="http://localhost:7777",
    data_plane="ate_env",  # or "router"
)

tasks = [
    Task(id="task-1", image="docker.io/library/python:3.11"),
    Task(id="task-2", image="docker.io/library/python:3.11"),
]

with SandboxFleet(config) as fleet:
    sandboxes = fleet.acquire(tasks)
    try:
        for task, sb in zip(tasks, sandboxes):
            res = sb.exec("python3 -c 'print(\"hello world\")'")
            print(f"Task {task.id}: {res.stdout.strip()} (exit_code={res.exit_code})")
    finally:
        fleet.release(sandboxes)
```

---

## 2. Asynchronous Non-Blocking Rollouts (VeRL / vLLM)

In RL post-training (e.g., GRPO), candidate generation overlaps with sandbox pre-warming to hide provisioning latency:

```python
import asyncio
from ate_env import AsyncSandboxFleet, FleetConfig, Task

async def run_rollouts():
    config = FleetConfig(
        backend="substrate",
        data_plane="ate_env",
        endpoint="http://ateapi.ate-system.svc.cluster.local:8080",
        grpc_endpoint="substrate-env.ate-system.svc.cluster.local:50051",
        batch_size=4,
        max_warmpool_replicas=4,
    )
    
    tasks = [Task(id=f"t-{i}", image="docker.io/library/python:3.11") for i in range(4)]
    
    fleet = AsyncSandboxFleet(config)
    await fleet.setup(tasks)

    # 1. Start acquiring pre-warmed sandboxes concurrently while LLM generates tokens
    warm_task = asyncio.create_task(fleet.acquire_batch(tasks))
    await asyncio.sleep(1.5)  # Simulate token generation
    sandboxes = await warm_task

    # 2. Asynchronously evaluate candidate rollouts
    for sb in sandboxes:
        async with sb:
            await sb.write_file_async("/testbed/calc.py", "def add(a, b): return a + b\n")
            res = await sb.exec_async(["python3", "-c", "import calc; assert calc.add(2, 3) == 5"])
            print(f"Sandbox {sb.sandbox_id}: ok={res.ok}")

    await fleet.teardown()

asyncio.run(run_rollouts())
```

---

## 3. Data Plane Options

| `data_plane` | Protocol | Target | Description |
| :--- | :--- | :--- | :--- |
| **`"ate_env"`** *(default)* | gRPC | `substrate-env:50051` | Native in-guest `ProcessService` and `FileSystemService` streaming. |
| **`"router"`** | HTTP | `atenet-router:8080` | Reverse proxy routing with `ate-target-actor` HTTP headers. |

---

## 4. End-to-End VeRL + Ray Demo on GKE

A full runnable example with Ray actors and GRPO advantage optimization is located in [`examples/verl_swebench`](../../examples/verl_swebench):

* **Local hermetic run**:
  ```bash
  python examples/verl_swebench/async_verl_swebench_pipeline.py --backend mock --num-iters 2 --group-size 2
  ```
* **Production RayJob on GKE**:
  ```bash
  kubectl apply -f examples/verl_swebench/ray-job.async-verl.yaml
  ```
  See [`examples/verl_swebench/README.md`](../../examples/verl_swebench/README.md) for full deployment instructions.
