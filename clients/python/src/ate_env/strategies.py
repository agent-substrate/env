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

from __future__ import annotations

import collections
import logging
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any, Callable, Dict, List, Optional

from .handle import SandboxHandle
from .types import Task

logger = logging.getLogger("sandbox_sdk.strategies")


def process_parallel(fleet: Any, tasks: List[Task], process_fn: Callable[[Task, SandboxHandle], Any],
                     concurrency: int) -> List[Any]:
    """Execute acquire -> process_fn -> release across tasks up to concurrency."""
    results: List[Any] = [None] * len(tasks)

    def _execute_single(task: Task) -> Any:
        handle = fleet.acquire(task)
        try:
            return process_fn(task, handle)
        finally:
            fleet.release(handle)

    if concurrency <= 1:
        for i, t in enumerate(tasks):
            try:
                results[i] = _execute_single(t)
            except Exception as e:
                logger.error("Task %s failed: %s", t.id, e)
                results[i] = e
        return results

    with ThreadPoolExecutor(max_workers=concurrency) as executor:
        futures = {executor.submit(_execute_single, t): i for i, t in enumerate(tasks)}
        for fut in as_completed(futures):
            i = futures[fut]
            try:
                results[i] = fut.result()
            except Exception as e:
                logger.error("Task %s failed: %s", tasks[i].id, e)
                results[i] = e

    return results


def run_none(fleet: Any, process_fn: Callable[[Task, SandboxHandle], Any],
             concurrency: int, teardown: bool = True) -> List[Any]:
    """No pre-warming: on-demand 1:1 acquisition per task."""
    try:
        return process_parallel(fleet, fleet.tasks, process_fn, concurrency)
    finally:
        if teardown:
            fleet.teardown()


def run_naive(fleet: Any, process_fn: Callable[[Task, SandboxHandle], Any],
              concurrency: int, teardown: bool = True) -> List[Any]:
    """Pre-warm every image up front, process all tasks in parallel, tear down."""
    try:
        fleet.setup()
        return process_parallel(fleet, fleet.tasks, process_fn, concurrency)
    finally:
        if teardown:
            fleet.teardown()


def run_sliding(fleet: Any, process_fn: Callable[[Task, SandboxHandle], Any],
                concurrency: int, teardown: bool = True) -> List[Any]:
    """Keep only a sliding window of image pools warm at a time (bounded footprint)."""
    fleet.preflight()
    fleet.plan()
    window = fleet.config.window_size or fleet.config.batch_size
    images = list(fleet.image_counts().keys())

    by_image: Dict[str, List[tuple[int, Task]]] = collections.defaultdict(list)
    for i, t in enumerate(fleet.tasks):
        by_image[t.image].append((i, t))

    results: List[Any] = [None] * len(fleet.tasks)
    try:
        for start in range(0, len(images), window):
            batch = images[start:start + window]
            fleet.warm_images(batch, wait=True)
            batch_pairs = [(i, t) for img in batch for (i, t) in by_image[img]]
            batch_tasks = [t for _i, t in batch_pairs]

            logger.info("Sliding window [%d..%d): %d image(s), %d task(s)",
                        start, start + len(batch), len(batch), len(batch_tasks))
            batch_results = process_parallel(fleet, batch_tasks, process_fn, concurrency)
            for (orig_idx, _t), r in zip(batch_pairs, batch_results):
                results[orig_idx] = r

            for img in batch:
                fleet.unwarm_image(img)
    finally:
        if teardown:
            fleet.teardown()

    return results


def run_pipelined(fleet: Any, process_fn: Callable[[Task, SandboxHandle], Any],
                  concurrency: int, teardown: bool = True) -> List[Any]:
    """
    Double-buffered pipelined sliding window.
    
    While window N tasks run, prefetch window N+1 in the background to overlap
    image pull or snapshot restore with GPU rollout time.
    """
    fleet.preflight()
    fleet.plan()
    window = fleet.config.window_size or fleet.config.batch_size
    images = list(fleet.image_counts().keys())

    by_image: Dict[str, List[tuple[int, Task]]] = collections.defaultdict(list)
    for i, t in enumerate(fleet.tasks):
        by_image[t.image].append((i, t))

    results: List[Any] = [None] * len(fleet.tasks)
    batches = [images[s:s + window] for s in range(0, len(images), window)]

    prefetch_executor = ThreadPoolExecutor(max_workers=1)
    try:
        if batches:
            fleet.warm_images(batches[0], wait=True)

        for n, batch in enumerate(batches):
            # Asynchronously prefetch window N+1
            nxt_future = (
                prefetch_executor.submit(fleet.warm_images, batches[n + 1], wait=True)
                if n + 1 < len(batches) else None
            )

            batch_pairs = [(i, t) for img in batch for (i, t) in by_image[img]]
            batch_tasks = [t for _i, t in batch_pairs]

            logger.info("Pipelined window [%d/%d]: %d image(s), %d task(s)",
                        n + 1, len(batches), len(batch), len(batch_tasks))
            batch_results = process_parallel(fleet, batch_tasks, process_fn, concurrency)
            for (orig_idx, _t), r in zip(batch_pairs, batch_results):
                results[orig_idx] = r

            # Unwarm batch N before awaiting next batch to maintain <= 2 windows bound
            for img in batch:
                fleet.unwarm_image(img)

            if nxt_future is not None:
                nxt_future.result()
    finally:
        prefetch_executor.shutdown(wait=True)
        if teardown:
            fleet.teardown()

    return results


STRATEGIES: Dict[str, Callable[..., List[Any]]] = {
    "none": run_none,
    "naive": run_naive,
    "sliding": run_sliding,
    "pipelined": run_pipelined,
}
