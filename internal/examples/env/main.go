// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command env demonstrates the gRPC EnvironmentService API lifecycle:
// creating, getting, suspending, and deleting an environment.
//
// It expects a running ate-env-api service (or port-forward):
//
//	kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	var (
		addr     string
		atespace string
		template string
	)
	flag.StringVar(&addr, "addr", "localhost:7777", "address of the ate-env-api gRPC service")
	flag.StringVar(&atespace, "atespace", "ate-env", "Substrate atespace")
	flag.StringVar(&template, "template", "default-template", "ActorTemplate name")
	flag.Parse()

	id := "env-" + uuid.NewString()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connecting to %s: %v", addr, err)
	}
	defer conn.Close()

	client := ateenvv1alpha.NewEnvironmentServiceClient(conn)

	// 1. Create the environment.
	fmt.Printf("1. Creating environment %q (template: %s, atespace: %s)...\n", id, template, atespace)
	createResp, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
		Id:       id,
		Atespace: atespace,
		Template: &ateenvv1alpha.Template{
			Name:     template,
			Atespace: atespace,
		},
	})
	if err != nil {
		log.Fatalf("CreateEnvironment failed: %v", err)
	}
	env := createResp.GetEnvironment()
	fmt.Printf("   Created: id=%s, atespace=%s, status=%v\n\n", env.GetId(), env.GetAtespace(), env.GetStatus())

	// 2. Get environment details.
	fmt.Printf("2. Getting environment %q...\n", id)
	getResp, err := client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{
		Id:       id,
		Atespace: atespace,
	})
	if err != nil {
		log.Fatalf("GetEnvironment failed: %v", err)
	}
	env = getResp.GetEnvironment()
	fmt.Printf("   Retrieved: id=%s, atespace=%s, template=%s (atespace: %s), status=%v\n\n",
		env.GetId(), env.GetAtespace(), env.GetTemplate().GetName(), env.GetTemplate().GetAtespace(), env.GetStatus())

	// 3. Suspend the environment.
	fmt.Printf("3. Suspending environment %q...\n", id)
	if _, err := client.SuspendEnvironment(ctx, &ateenvv1alpha.SuspendEnvironmentRequest{
		Id:       id,
		Atespace: atespace,
	}); err != nil {
		log.Fatalf("SuspendEnvironment failed: %v", err)
	}
	fmt.Printf("   Suspended environment %q successfully.\n\n", id)

	// 4. Verify status after suspension.
	getResp, err = client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{
		Id:       id,
		Atespace: atespace,
	})
	if err != nil {
		log.Fatalf("GetEnvironment after suspend failed: %v", err)
	}
	fmt.Printf("   Status after suspend: %v\n\n", getResp.GetEnvironment().GetStatus())

	// 5. Delete the environment.
	fmt.Printf("5. Deleting environment %q...\n", id)
	if _, err := client.DeleteEnvironment(ctx, &ateenvv1alpha.DeleteEnvironmentRequest{
		Id:       id,
		Atespace: atespace,
	}); err != nil {
		log.Fatalf("DeleteEnvironment failed: %v", err)
	}
	fmt.Printf("   Deleted environment %q successfully.\n", id)
}
