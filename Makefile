.PHONY: build install test vet clean

build:
	go build ./...

# Install sbx and sbx-api to $GOBIN (or $GOPATH/bin).
install:
	go install github.com/agent-substrate/sandbox/cmd/sbx github.com/agent-substrate/sandbox/cmd/sbx-api

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
