# VeRL + Ray + SWE-bench on Sandbox SDK Demo

This demo demonstrates **Reinforcement Learning (RL) post-training** with Group Relative Policy Optimization (GRPO) using **VeRL**, **Ray Core**, and **SWE-bench** on the **Unified Sandbox SDK (`sandbox-sdk`)**.

---

## Architecture Overview

```
                                    Ray Cluster
┌─────────────────────────────────────────────────────────────────────────────────┐
│                                                                                 │
│   [ VeRL Trainer Worker ]                        [ VeRL Sampler Worker ]        │
│   - GRPO policy optimization                     - Rollout candidate generation │
│   - Weight synchronization (FSDP) <============= - Simulates vLLM sampler       │
│                                                                                 │
│                                         │                                       │
│                         Dispatches K candidate rollouts                         │
│                                         ▼                                       │
│                 [ Ray Parallel Sandbox Evaluators (1..K) ]                      │
│                                         │                                       │
└─────────────────────────────────────────┼───────────────────────────────────────┘
                                          │
                            Sandbox SDK (SandboxFleet)
                                          │
            ┌─────────────────────────────┴─────────────────────────────┐
            ▼ (Substrate Engine)                                        ▼ (Kubernetes Engine)
  Agent Substrate ate-system                                   Standard GKE Pods / CRDs
  - Instant claim from Golden Snapshot                         - Ready pods from WarmPool
  - Node-local paused actors (~1s resume)                      - Persistent bash websocket
  - streaming /process or ate-env gRPC                         - router-free pod exec
```

---

## Key Benefits of `sandbox-sdk` over Raw Client Scripts

1. **Warm Pool Provisioning**: Automatically pre-warms paused actors or ready pods ahead of iteration barriers.
2. **Pipelined Double-Buffered Windowing**: Hides image pull and snapshot restore latency behind GPU rollout generation.
3. **Pluggable Execution**: Seamlessly switch between `backend="mock"`, `backend="substrate"`, and `backend="kubernetes"`.
4. **Clean Run Isolation & Teardown**: Automatically attaches run labels and reaps orphaned actors upon job completion.

---

## Running the Demo

### 1. Local Hermetic Run (No External Cluster Needed)
```bash
python3 async_verl_swebench_pipeline.py --backend mock --num-iters 2 --group-size 2
```

### 2. Live Substrate Run on GKE
Demonstrates overlapping vLLM token decoding with asynchronous batch sandbox acquisition (`AsyncSandboxFleet.acquire_batch`):

**Mode A: HTTP Reverse Proxy (`--data-plane router`)**
```bash
python3 async_verl_swebench_pipeline.py \
  --backend substrate \
  --data-plane router \
  --task-type smoke \
  --num-iters 3 \
  --group-size 4
```

**Mode B: Native gRPC client (`--data-plane ate_env`)**
Uses the official `ate-env-client` package with native non-blocking coroutines:
```bash
python3 async_verl_swebench_pipeline.py \
  --backend substrate \
  --data-plane ate_env \
  --task-type smoke \
  --num-iters 3 \
  --group-size 4
```

### 3. Deploying as a RayJob on GKE
Submit the async pipeline to KubeRay:
```bash
kubectl apply -f ray-job.async-verl.yaml
```
