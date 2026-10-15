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
# unmodified images at /opt/oh. It brings everything it runs on: a relocatable
# Python and the agent-server's packages. From the image it is mounted into it
# uses only the kernel and /bin/sh, which the agent-server runs commands with.
#
# Why not the binary in the published ghcr.io/openhands/agent-server images:
# it is a PyInstaller build made on Debian bookworm and needs glibc 2.36, so it
# fails on older distributions, among them Ubuntu 22.04 (glibc 2.35), the base
# of many coding-task images. Starting it through its own image's loader does
# not help, because PyInstaller re-executes itself. Here:
#
# - python-build-standalone CPython, built for glibc 2.17 and relocatable, so
#   it runs wherever the layer is mounted;
# - openhands-agent-server installed by pip on Debian bullseye (glibc 2.31),
#   so pip only picks wheels that run on glibc 2.31 and newer.
#
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t <registry>/oh-agent-server-layer:1.55.0 --push .
#
# Override AGENT_SERVER_VERSION to build another release. The image has no
# shell or entrypoint of its own; it is only ever mounted.

# The interpreter is fetched and verified in a small stage of its own, so the
# build stage needs no package manager.
FROM curlimages/curl:8.16.0 AS fetch

ARG TARGETARCH
ARG PBS_RELEASE=20261013
ARG PYTHON_VERSION=3.12.15
ARG PBS_SHA256_AMD64=5d546fc004fc2435fa142eb8f42ed8de5eb4725963dc22f7e6671014a80eb560
ARG PBS_SHA256_ARM64=34a43010cc05982d8b5debbad7d09f7b1200117ebf018f7acfd656895e408205

RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) arch=x86_64; sum="${PBS_SHA256_AMD64}" ;; \
      arm64) arch=aarch64; sum="${PBS_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    f="cpython-${PYTHON_VERSION}+${PBS_RELEASE}-${arch}-unknown-linux-gnu-install_only.tar.gz"; \
    curl -fsSLo "/tmp/${f}" "https://github.com/astral-sh/python-build-standalone/releases/download/${PBS_RELEASE}/${f}"; \
    echo "${sum}  /tmp/${f}" | sha256sum -c -; \
    mkdir -p /tmp/oh; \
    tar -xzf "/tmp/${f}" -C /tmp/oh

FROM debian:bullseye-slim AS build

ARG AGENT_SERVER_VERSION=1.55.0

COPY --from=fetch /tmp/oh/ /opt/oh/

# Wheels wherever they exist. The few pure-Python packages that ship only as
# source (func-timeout) build fine; a native one would fail here, because the
# slim image has no compiler, rather than link against this builder.
RUN /opt/oh/python/bin/python3 -m pip install --no-cache-dir --prefer-binary \
      "openhands-agent-server==${AGENT_SERVER_VERSION}" \
 && /opt/oh/python/bin/python3 -c "import openhands.agent_server" \
 && find /opt/oh/python -name __pycache__ -prune -exec rm -rf {} +

FROM scratch
COPY --from=build /opt/oh/ /
