// Command grpc demonstrates the ateenv.v1alpha1 guest data plane
// through ate-env-api: create an environment, run a bounded command,
// stream a file up and down, and drive a background process by polling
// byte offsets — the long-running-operation loop that survives dropped
// connections and environment suspend/resume.
//
// It expects a port-forward to the ate-env-api service:
//
//	kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/agent-substrate/env/env"
	ateenvv1 "github.com/agent-substrate/env/proto/ateenv/v1"
	pb "github.com/agent-substrate/env/proto/ateenv/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func main() {
	ctx := context.Background()

	conn, err := grpc.NewClient("localhost:7777", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connecting to ate-env-api: %v", err)
	}
	defer conn.Close()

	// 1. Create an environment over the lifecycle API.
	client, err := env.NewClient(env.ClientOptions{GRPCConn: conn})
	if err != nil {
		log.Fatal(err)
	}
	e, err := client.Create(ctx, &ateenvv1.CreateEnvironmentRequest{Id: "grpc-demo"})
	if err != nil {
		log.Fatalf("creating environment: %v", err)
	}
	defer e.Delete(ctx)

	// Every data-plane call names its environment in request metadata;
	// the api service proxies it to that environment's guest.
	ctx = metadata.AppendToOutgoingContext(ctx, "ate-env-id", "grpc-demo")
	procs := pb.NewProcessClient(conn)
	files := pb.NewFileSystemClient(conn)

	// 2. A bounded command: outcome in-band, never a gRPC error.
	res, err := procs.Exec(ctx, &pb.ExecRequest{Command: "uname -a"})
	if err != nil {
		log.Fatalf("Exec: %v", err)
	}
	fmt.Printf("uname: %s", res.GetStdout())

	// 3. Stream a file up, then back down.
	w, err := files.WriteFile(ctx)
	if err != nil {
		log.Fatalf("WriteFile: %v", err)
	}
	if err := w.Send(&pb.WriteFileRequest{Path: "/tmp/notes.txt", Data: []byte("hello from the gRPC data plane\n")}); err != nil {
		log.Fatalf("WriteFile send: %v", err)
	}
	if _, err := w.CloseAndRecv(); err != nil {
		log.Fatalf("WriteFile close: %v", err)
	}
	r, err := files.ReadFile(ctx, &pb.ReadFileRequest{Path: "/tmp/notes.txt"})
	if err != nil {
		log.Fatalf("ReadFile: %v", err)
	}
	for {
		chunk, err := r.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Fatalf("ReadFile recv: %v", err)
		}
		fmt.Printf("notes.txt: %s", chunk.GetData())
	}

	// 4. A background process, driven as a long-running operation: the
	// durable handles are the process id and byte offsets, so this loop
	// works no matter how many connections — or suspend/resume cycles —
	// happen along the way.
	start, err := procs.Start(ctx, &pb.StartRequest{
		Command: "for i in 1 2 3; do echo tick $i; sleep 1; done",
	})
	if err != nil {
		log.Fatalf("Start: %v", err)
	}
	var stdoutOff, stderrOff int64
	for {
		out, err := procs.Get(ctx, &pb.GetRequest{
			ProcessId:    start.GetProcessId(),
			StdoutOffset: stdoutOff,
			StderrOffset: stderrOff,
			WaitMs:       2000, // bounded long-poll
		})
		if err != nil {
			log.Fatalf("Get: %v", err)
		}
		fmt.Print(string(out.GetStdout()))
		stdoutOff = out.GetStdoutOffset() + int64(len(out.GetStdout()))
		stderrOff = out.GetStderrOffset() + int64(len(out.GetStderr()))
		if exited := out.GetProcess().GetExited(); exited != nil {
			fmt.Printf("process exited with code %d\n", exited.GetExitCode())
			return
		}
	}
}
