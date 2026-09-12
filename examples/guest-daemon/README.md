# Agent Substrate Guest Daemon Example

This directory contains an example implementation and guide for running an in-container **Guest Daemon** for Agent Substrate environments.

It demonstrates how to configure and run the gRPC guest services using the **`github.com/agent-substrate/env/guest`** package, exposing **`ProcessService`** (asynchronous process execution, stdin/stdout/stderr streaming, signals) and **`FileSystemService`** (chunked streaming file manipulation).

---

## Architecture

```mermaid
flowchart TD
    Client(["AI Agent / Substrate Router"])

    subgraph Container["Actor"]
        subgraph GuestDaemon["Guest Daemon (:8080)"]
            ProcSvc["ProcessService"]
            FSSvc["FileSystemService"]
            Tracker["Process Tracker<br/>"]
            
            ProcSvc --> Tracker
        end

        subgraph OS["Linux OS & Storage"]
            ProcTree["Linux Process Group<br/>(Child Subprocesses)"]
            LogDir[("Log Spooling<br/>/var/log/ate-jobs/*.[stdout|stderr]")]
            Workspace[("Sandbox Root<br/>/workspace")]
        end

        Tracker -->|"Spawns & Monitors"| ProcTree
        ProcTree -.->|"Pipes stdout/stderr"| LogDir
        Tracker -->|"Reads offsets"| LogDir
        FSSvc <-->|"Streamed Chunks<br/>(Sandbox Confined)"| Workspace
    end

    Client <-->|"gRPC (HTTP/2)"| GuestDaemon
```

---

## Directory Contents

- **`main.go`**: Example demonstrating how to configure and launch the guest daemon using `guest.NewServer`.
- **`main_test.go`**: Integration test validating daemon startup, file uploading, process execution, and log streaming.
- **`Dockerfile`**: Multi-stage container build producing a minimal image with the compiled `guest-daemon` binary.

---

## 1. Running the Daemon Locally

To test the daemon on your local machine:

```bash
go run ./examples/guest-daemon --listen=:8080 --log-dir=/tmp/ate-jobs --workspace=/tmp/workspace
```

---

## 2. Running the Integration Tests

Run the full end-to-end test suite:

```bash
go test -v -race ./examples/guest-daemon/...
```

---

## 3. Testing with `grpcurl`

Since reflection is enabled, you can interact directly with the running daemon using [`grpcurl`](https://github.com/fullstorydev/grpcurl):

```bash
# If grpcurl is not in your PATH, you can install it or invoke ~/go/bin/grpcurl:
go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest
export PATH=$PATH:$(go env GOPATH)/bin
```

### Start a Background Command
```bash
grpcurl -plaintext -d '{"command": ["echo", "Hello Substrate!"]}' \
  localhost:8080 ateenv.v1alpha.ProcessService/StartProcess
```

Add `"stdin": true` to open a stdin pipe and `"timeout": "30s"` to bound the run time.

### Inspect Process State
```bash
grpcurl -plaintext -d '{"process_id": "<process-id-from-start>"}' \
  localhost:8080 ateenv.v1alpha.ProcessService/GetProcess
```

### Stream Real-Time Output
```bash
grpcurl -plaintext -d '{"process_id": "<process-id>", "follow": true}' \
  localhost:8080 ateenv.v1alpha.ProcessService/StreamProcessOutput
```

The stream ends with an `exit` message carrying the final `Process` once the command has exited.

### Write to stdin (Streamed)
```bash
echo '{"process_id": "<process-id>", "data": "aGVsbG8K", "close": true}' | \
  grpcurl -plaintext -d @ localhost:8080 ateenv.v1alpha.ProcessService/WriteProcessInput
```

### Signal a Process
```bash
grpcurl -plaintext -d '{"process_id": "<process-id>", "signal": "SIGNAL_TERM"}' \
  localhost:8080 ateenv.v1alpha.ProcessService/SignalProcess
```

### Write a File (Streamed)
```bash
echo '{"path": "hello.txt", "chunk": "SGVsbG8gU3Vic3RyYXRlIQo=", "mode": 420}' | \
  grpcurl -plaintext -d @ localhost:8080 ateenv.v1alpha.FileSystemService/WriteFile
```

Add `"seek_offset": N` to keep the existing content and write starting at byte `N` instead of replacing the file.

### Read a File (Streamed)
```bash
grpcurl -plaintext -d '{"path": "hello.txt"}' \
  localhost:8080 ateenv.v1alpha.FileSystemService/ReadFile
```

---

## 4. Building the Container Image

Build the minimal container image:
```bash
docker build -t us-docker.pkg.dev/my-project/guest-daemon:latest -f examples/guest-daemon/Dockerfile .
```
