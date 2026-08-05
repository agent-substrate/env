SBX_IMAGE_REPO ?= us-central1-docker.pkg.dev/agent-substrate/sandbox

.PHONY: build install test vet clean images

build:
	go build ./...

# Install sbx and sbx-api to $GOBIN (or $GOPATH/bin).
install:
	go install ./cmd/...

test:
	go test ./...

vet:
	go vet ./...

images:
	@echo "Building and pushing container images to $(SBX_IMAGE_REPO)..."
	@guest_img=$$(KO_DOCKER_REPO=$(SBX_IMAGE_REPO)/sbx-guest ko build --bare ./cmd/sbx-guest); \
	api_img=$$(KO_DOCKER_REPO=$(SBX_IMAGE_REPO)/sbx-api ko build --bare ./cmd/sbx-api); \
	version=$$(go list -m -f '{{.Version}}' github.com/agent-substrate/substrate); \
	commit=$${version##*-}; \
	tmpdir=$$(mktemp -d); \
	echo "Cloning substrate@$$commit to build ateom-gvisor..."; \
	git clone --filter=blob:none https://github.com/agent-substrate/substrate "$$tmpdir" >/dev/null 2>&1; \
	git -C "$$tmpdir" checkout "$$commit" >/dev/null 2>&1; \
	ateom_img=$$(cd "$$tmpdir" && KO_DOCKER_REPO=$(SBX_IMAGE_REPO)/ateom-gvisor ko build --bare ./cmd/ateom-gvisor); \
	rm -rf "$$tmpdir"; \
	echo "Updating README.md with published image SHAs..."; \
	python3 -c 'import re; g="'"$$guest_img"'"; a="'"$$api_img"'"; t="'"$$ateom_img"'"; r=open("README.md").read(); r=re.sub(r"(--guest-image\s+)\S+", r"\1"+g, r); r=re.sub(r"(--api-image\s+)\S+", r"\1"+a, r); r=re.sub(r"(--ateom-image\s+)\S+", r"\1"+t, r); open("README.md","w").write(r)'; \
	echo "Published images updated in README.md:" && \
	echo "  --guest-image $$guest_img" && \
	echo "  --api-image   $$api_img" && \
	echo "  --ateom-image $$ateom_img"

clean:
	rm -rf bin
