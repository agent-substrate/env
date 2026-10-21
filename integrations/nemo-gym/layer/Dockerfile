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
# The NeMo runtime layer: a relocatable CPython with NeMo Skills' local sandbox
# server and NeMo Gym's exec server installed, built as an OCI image with
# nothing else in it. Mounted read-only into an environment (ate-env manifest
# template --layer nemo=<image>=/opt/nemo), the guest starts either server as
# a sidecar; the task image contributes only the kernel and its filesystem.
# Same recipe as integrations/openhands/layer: python-build-standalone plus
# hash-pinned wheels installed with uv. linux/amd64 and linux/arm64.
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
    mkdir -p /tmp/nemo; \
    tar -xzf "/tmp/${f}" -C /tmp/nemo

FROM ghcr.io/astral-sh/uv:0.11.18@sha256:78bc42400d77b0678ba95765305c826652ed5431f399257271dda681d0318f03 AS uv

FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS build
ARG TARGETARCH
ENV UV_NO_CACHE=1 UV_PYTHON_DOWNLOADS=never
COPY --from=uv /uv /usr/local/bin/uv
COPY --from=fetch /tmp/nemo/ /opt/nemo/
COPY requirements.txt build-constraints.txt /tmp/lock/
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN case "${TARGETARCH}" in \
      amd64) platform=x86_64-manylinux_2_31 ;; \
      arm64) platform=aarch64-manylinux_2_31 ;; \
      *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && uv pip install --index-url https://pypi.org/simple --require-hashes \
      --python /opt/nemo/python/bin/python3 --python-platform "${platform}" --python-version 3.12 \
      --target /opt/nemo/python/lib/python3.12/site-packages \
      --build-constraints /tmp/lock/build-constraints.txt -r /tmp/lock/requirements.txt
# The entry points and the vendored servers live in site-packages, so
# python3 -m nemo_runtime.<server> works wherever the layer is mounted.
COPY nemo_runtime/ /opt/nemo/python/lib/python3.12/site-packages/nemo_runtime/
COPY third_party/ /opt/nemo/python/lib/python3.12/site-packages/nemo_runtime/third_party/
RUN /opt/nemo/python/bin/python3 -c "import flask, psutil, IPython, traitlets; import nemo_runtime.sandbox_server, nemo_runtime.exec_server; nemo_runtime.sandbox_server.load_app()" \
 && find /opt/nemo/python -name __pycache__ -prune -exec rm -rf {} + \
 && /opt/nemo/python/bin/python3 -m compileall -q -j 0 --invalidation-mode unchecked-hash /opt/nemo/python/lib

FROM scratch
COPY --from=build /opt/nemo/ /
