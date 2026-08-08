ATE_ENV_IMAGE_REPO ?= us-docker.pkg.dev/agent-substrate-env/ate-env-images

.PHONY: build install test vet clean images

build:
	go build ./...

# Install ate-env and ate-env-api to $GOBIN (or $GOPATH/bin).
install:
	go install ./cmd/...

test:
	go test ./...

vet:
	go vet ./...

images:
	@echo "Building and pushing container images to $(ATE_ENV_IMAGE_REPO)..."
	@guest_img=$$(KO_DOCKER_REPO=$(ATE_ENV_IMAGE_REPO)/ate-env-guest ko build --bare ./cmd/ate-env-guest); \
	api_img=$$(KO_DOCKER_REPO=$(ATE_ENV_IMAGE_REPO)/ate-env-api ko build --bare ./cmd/ate-env-api); \
	echo "Updating README.md with published image SHAs..."; \
	python3 -c 'import re; g="'"$$guest_img"'"; a="'"$$api_img"'"; r=open("README.md").read(); r=re.sub(r"(--guest-image\s+)[^\s\\]+", r"\1"+g, r); r=re.sub(r"(--api-image\s+)[^\s\\]+", r"\1"+a, r); open("README.md","w").write(r)'; \
	echo "Published images updated in README.md:" && \
	echo "  --guest-image $$guest_img" && \
	echo "  --api-image   $$api_img"

clean:
	rm -rf bin
