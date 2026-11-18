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

ATE_ENV_IMAGE_REPO ?= localhost:5001/ate-env
SNAPSHOTS_BUCKET ?=

# Baked into the ate-env CLI by build-cli (release.yaml does the same with the
# published digests): `ate-env --version`, and the defaults of manifest
# --api-image and manifest template --guest-image.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ATE_ENV_API_IMAGE ?=
ATE_ENV_GUEST_IMAGE ?=
CLI_LDFLAGS := -X main.version=$(VERSION) -X main.defaultAPIImage=$(ATE_ENV_API_IMAGE) -X main.defaultGuestImage=$(ATE_ENV_GUEST_IMAGE)

.PHONY: build build-cli build-install install-access install test vet clean images python-protos python-test verify-boilerplate verify-gofmt

build:
	go build ./...

# Build bin/ate-env with the version and, if ATE_ENV_API_IMAGE /
# ATE_ENV_GUEST_IMAGE are set, image defaults baked in.
build-cli:
	go build -trimpath -ldflags "$(CLI_LDFLAGS)" -o bin/ate-env ./cmd/ate-env

# Install into the current kubectl context; Substrate must already be running.
build-install:
	@test -n "$(SNAPSHOTS_BUCKET)" || { echo "SNAPSHOTS_BUCKET is required (e.g. gs://bucket/prefix/)" >&2; exit 1; }
	@command -v ko >/dev/null && command -v kubectl >/dev/null && command -v kubectl-ate >/dev/null && command -v jq >/dev/null
	$(MAKE) install-access
	$(MAKE) build-cli images
	./bin/ate-env manifest \
		--api-image "$$(cat bin/ate-env-api.image)" \
		--worker-image "$$(cat bin/ateworker-gvisor.image)" > bin/deployment.yaml
	./bin/ate-env manifest template \
		--guest-image "$$(cat bin/ate-env-guest.image)" \
		--snapshots-bucket "$(SNAPSHOTS_BUCKET)" > bin/template.yaml
	kubectl apply -f bin/deployment.yaml
	kubectl -n ate-env rollout status deployment/ate-env-api --timeout=300s
	kubectl-ate create actor-template -f bin/template.yaml

# kubectl-ate must run as an administrator; the API itself only gets editor.
install-access:
	@command -v kubectl-ate >/dev/null && command -v jq >/dev/null
	@mkdir -p bin
	kubectl-ate get atespaces -o json > bin/atespaces.json
	@set -eu; \
	exists=$$(jq -r 'any(.atespaces[]?; .metadata.name == "ate-env")' bin/atespaces.json); \
	if test "$$exists" = false; then \
		kubectl-ate create atespace ate-env; \
	fi
	@set -eu; \
	policy=$$(kubectl-ate get access-policy -a ate-env -o json); \
	if test -n "$$policy"; then action=update; else action=create; policy='{}'; fi; \
	printf '%s\n' "$$policy" | jq --arg member 'user:system:serviceaccount:ate-env:default' \
		'.bindings = ((.bindings // []) | if any(.[]; (.role == "editor" or .role == "owner") and any(.members[]?; . == $$member)) then . elif any(.[]; .role == "editor") then map(if .role == "editor" then .members = ((.members // []) + [$$member]) else . end) else . + [{role: "editor", members: [$$member]}] end)' \
		> bin/access-policy.json; \
	kubectl-ate "$$action" access-policy -a ate-env -f bin/access-policy.json

# Install ate-env and ate-env-api to $GOBIN (or $GOPATH/bin).
install:
	go install ./cmd/...

test:
	go test ./...

vet:
	go vet ./...

verify-boilerplate:
	hack/verify/boilerplate.sh

# Fail when any tracked Go file is not gofmt-formatted; prints the offenders.
verify-gofmt:
	@out=$$(gofmt -l $$(git ls-files '*.go')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# Regenerate the committed Python gRPC stubs (requires grpcio-tools; see
# clients/python/README.md).
python-protos:
	./clients/python/scripts/gen-protos.sh

# Run the Python client tests (requires an environment with the dev extra
# installed: pip install -e 'clients/python[dev]').
python-test:
	cd clients/python && python3 -m pytest

# The worker image is built from the Substrate module go.mod pins, in its own
# module directory so its .ko.yaml and go.sum apply. --base-import-paths and
# the version stamp match what Substrate's `ate-setup publish worker-images`
# does, so the image lands at $(ATE_ENV_IMAGE_REPO)/ateworker-gvisor.
SUBSTRATE_DIR = $(shell go list -m -f '{{.Dir}}' go.ate.dev/substrate)
SUBSTRATE_VERSION = $(shell go list -m -f '{{.Version}}' go.ate.dev/substrate)

images:
	@echo "Building and pushing container images to $(ATE_ENV_IMAGE_REPO)..."
	@mkdir -p bin
	@set -eu; \
	KO_DOCKER_REPO=$(ATE_ENV_IMAGE_REPO)/ate-env-guest ko build --bare ./cmd/ate-env-guest > bin/ate-env-guest.image; \
	KO_DOCKER_REPO=$(ATE_ENV_IMAGE_REPO)/ate-env-api ko build --bare ./cmd/ate-env-api > bin/ate-env-api.image; \
	(cd "$(SUBSTRATE_DIR)" && KO_DOCKER_REPO=$(ATE_ENV_IMAGE_REPO) \
		GOFLAGS=-ldflags=-X=go.ate.dev/substrate/internal/version.Version=$(SUBSTRATE_VERSION) \
		ko build --base-import-paths ./cmd/ateworker-gvisor) > bin/ateworker-gvisor.image; \
	echo "Published:" && \
	echo "  --guest-image  $$(cat bin/ate-env-guest.image)" && \
	echo "  --api-image    $$(cat bin/ate-env-api.image)" && \
	echo "  --worker-image $$(cat bin/ateworker-gvisor.image)"

clean:
	rm -rf bin
