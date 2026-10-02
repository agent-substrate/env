import pytest
from ate_env.types import EnvironmentSpec, PlacementSpec, ResourceLimits, Task


def test_environment_spec_template_key_hashing():
    spec1 = EnvironmentSpec(
        image="us-central1-docker.pkg.dev/proj/repo/swe-bench:latest",
        runtime_bundle="substrate-env:v1",
        placement=PlacementSpec(worker_family="c2")
    )
    spec2 = EnvironmentSpec(
        image="us-central1-docker.pkg.dev/proj/repo/swe-bench:latest",
        runtime_bundle="substrate-env:v1",
        placement=PlacementSpec(worker_family="c2")
    )
    # Different CPU family must produce a different template key to protect snapshot restores
    spec3 = EnvironmentSpec(
        image="us-central1-docker.pkg.dev/proj/repo/swe-bench:latest",
        runtime_bundle="substrate-env:v1",
        placement=PlacementSpec(worker_family="c3")
    )

    assert spec1.template_key() == spec2.template_key()
    assert spec1.template_key() != spec3.template_key()
    assert "c2" not in spec1.template_key() or spec1.template_key().startswith("tmpl-")


def test_task_metadata():
    task = Task(id="pytest-1", image="pytest-img", metadata={"repo": "pytest-dev/pytest"})
    assert task.id == "pytest-1"
    assert task.metadata["repo"] == "pytest-dev/pytest"
