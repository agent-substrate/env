.PHONY: build install test vet clean

build:
	go build ./...
	go build -o bin/ssbx ./cmd/ssbx
	go build -o bin/ssbx-api ./cmd/ssbx-api

# Install ssbx and ssbx-api to $GOBIN (or $GOPATH/bin).
install:
	go install github.com/agent-substrate/sandbox/cmd/ssbx github.com/agent-substrate/sandbox/cmd/ssbx-api

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
