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

# A runtime layer that carries the OpenHands agent-server, for mounting into
# unmodified images at /opt/oh. It brings its own relocatable Python and the
# agent-server's packages. From the image it is mounted into it uses the
# kernel, the C library and /bin/sh, which the agent-server runs commands
# with. The C library must be glibc 2.31 or newer: Debian 11, Ubuntu 20.04,
# RHEL 9 and later work; Alpine (musl), Ubuntu 18.04 and CentOS 7 do not.
#
# Why not the binary in the published ghcr.io/openhands/agent-server images:
# it is a PyInstaller build made on Debian bookworm and needs glibc 2.36, so it
# fails on older distributions, among them Ubuntu 22.04 (glibc 2.35), the base
# of many coding-task images. Starting it through its own image's loader does
# not help, because PyInstaller re-executes itself. Here:
#
# - python-build-standalone CPython, built for glibc 2.17 and relocatable, so
#   it runs wherever the layer is mounted;
# - the agent-server and its dependencies from requirements.txt, hash-locked,
#   installed by uv for the target platform manylinux_2_31, so only wheels
#   that run on glibc 2.31 and newer qualify, whatever the build stage's own
#   glibc. The one source-only dependency (func-timeout, pure Python) is
#   built with the hash-locked setuptools from build-constraints.txt.
#
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t <registry>/oh-agent-server-layer:1.55.0 --push .
#
# Another agent-server release: change the three versions in requirements.in
# and run ./lock.sh, then build with --build-arg AGENT_SERVER_VERSION=<version>,
# which is checked against the installed agent-server, sdk and tools. The image
# has no shell or entrypoint of its own; it is only ever mounted. Base images are pinned by digest; update the tag and
# the digest together.

# The interpreter is fetched and verified in a small stage of its own.
FROM curlimages/curl:8.16.0@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6 AS fetch

ARG TARGETARCH
ARG PBS_RELEASE=20261013
ARG PYTHON_VERSION=3.12.15
ARG PBS_SHA256_AMD64=5d546fc004fc2435fa142eb8f42ed8de5eb4725963dc22f7e6671014a80eb560
ARG PBS_SHA256_ARM64=34a43010cc05982d8b5debbad7d09f7b1200117ebf018f7acfd656895e408205

SHELL ["/bin/ash", "-euo", "pipefail", "-c"]
RUN case "${TARGETARCH}" in \
      amd64) arch=x86_64; sum="${PBS_SHA256_AMD64}" ;; \
      arm64) arch=aarch64; sum="${PBS_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    f="cpython-${PYTHON_VERSION}+${PBS_RELEASE}-${arch}-unknown-linux-gnu-install_only.tar.gz"; \
    curl -fsSLo "/tmp/${f}" "https://github.com/astral-sh/python-build-standalone/releases/download/${PBS_RELEASE}/${f}"; \
    echo "${sum}  /tmp/${f}" | sha256sum -c -; \
    mkdir -p /tmp/oh; \
    tar -xzf "/tmp/${f}" -C /tmp/oh

FROM ghcr.io/astral-sh/uv:0.11.18@sha256:78bc42400d77b0678ba95765305c826652ed5431f399257271dda681d0318f03 AS uv

# Debian 13. Its own glibc does not matter: uv selects wheels for the target
# platform, and the slim image has no compiler, so a native package without a
# suitable wheel fails here instead of linking against this builder.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS build

ARG TARGETARCH
ARG AGENT_SERVER_VERSION=1.55.0
ENV UV_NO_CACHE=1 UV_PYTHON_DOWNLOADS=never

COPY --from=uv /uv /usr/local/bin/uv
COPY --from=fetch /tmp/oh/ /opt/oh/
COPY requirements.txt build-constraints.txt /tmp/lock/

# Precompiled with unchecked-hash bytecode: the layer is mounted read-only, so
# Python could not write a cache and would compile every module at each start;
# unchecked-hash .pyc files are used without looking at the sources' mtimes.
# func_timeout's py2_raise.py is Python 2 only and never imported on Python 3.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN case "${TARGETARCH}" in \
      amd64) platform=x86_64-manylinux_2_31 ;; \
      arm64) platform=aarch64-manylinux_2_31 ;; \
      *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && uv pip install --index-url https://pypi.org/simple --require-hashes \
      --python /opt/oh/python/bin/python3 --python-platform "${platform}" --python-version 3.12 \
      --target /opt/oh/python/lib/python3.12/site-packages \
      --build-constraints /tmp/lock/build-constraints.txt -r /tmp/lock/requirements.txt \
 && /opt/oh/python/bin/python3 -c "import importlib.metadata as m, openhands.agent_server; v = {p: m.version(p) for p in ('openhands-agent-server', 'openhands-sdk', 'openhands-tools')}; assert set(v.values()) == {'${AGENT_SERVER_VERSION}'}, v" \
 && find /opt/oh/python -name __pycache__ -prune -exec rm -rf {} + \
 && /opt/oh/python/bin/python3 -m compileall -q -j 0 --invalidation-mode unchecked-hash \
      -x 'func_timeout/py2_raise\.py$' /opt/oh/python/lib

FROM scratch
COPY --from=build /opt/oh/ /
