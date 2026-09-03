GOOGLE_CLOUD_PROJECT ?= $(shell gcloud config get-value project 2>/dev/null)
ATE_ENV_IMAGE_REPO ?= gcr.io/$(GOOGLE_CLOUD_PROJECT)

.PHONY: build install test vet clean images python-protos python-test

build:
	go build ./...

# Install ate-env and ate-env-api to $GOBIN (or $GOPATH/bin).
install:
	go install ./cmd/...

test:
	go test ./...

vet:
	go vet ./...

# Regenerate the committed Python gRPC stubs (requires grpcio-tools; see
# clients/python/README.md).
python-protos:
	./clients/python/scripts/gen-protos.sh

# Run the Python client tests (requires an environment with the dev extra
# installed: pip install -e 'clients/python[dev]').
python-test:
	cd clients/python && python3 -m pytest

images:
	@echo "Building and pushing container images to $(ATE_ENV_IMAGE_REPO)..."
	@guest_img=$$(KO_DOCKER_REPO=$(ATE_ENV_IMAGE_REPO)/ate-env-guest ko build --bare ./cmd/ate-env-guest | tail -n 1); \
	api_img=$$(KO_DOCKER_REPO=$(ATE_ENV_IMAGE_REPO)/ate-env-api ko build --bare ./cmd/ate-env-api | tail -n 1); \
	echo "Updating README.md with published image SHAs..."; \
	python3 -c 'import sys, re; g = sys.argv[1].split("/")[-1]; a = sys.argv[2].split("/")[-1]; r = open("README.md").read(); r = re.sub(r"(--guest-image\s+.*?" + re.escape("$$GOOGLE_CLOUD_PROJECT/") + r")\S+", r"\g<1>" + g, r); r = re.sub(r"(--api-image\s+.*?" + re.escape("$$GOOGLE_CLOUD_PROJECT/") + r")\S+", r"\g<1>" + a, r); open("README.md", "w").write(r)' "$$guest_img" "$$api_img"; \
	echo "Published images updated in README.md:" && \
	echo "  --guest-image $$guest_img" && \
	echo "  --api-image   $$api_img"

clean:
	rm -rf bin
