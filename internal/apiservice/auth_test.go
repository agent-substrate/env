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

package apiservice_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/apiservice"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestBearerAuthFromFileValidation(t *testing.T) {
	if _, err := apiservice.NewBearerAuth(""); err == nil {
		t.Fatal("NewBearerAuth with empty token: want error, got nil")
	}
	if _, err := apiservice.NewBearerAuthFromFile(""); err == nil {
		t.Fatal("NewBearerAuthFromFile with empty path: want error, got nil")
	}
	if _, err := apiservice.NewBearerAuthFromFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("NewBearerAuthFromFile with missing file: want error, got nil")
	}
	emptyFile := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(emptyFile, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := apiservice.NewBearerAuthFromFile(emptyFile); err == nil {
		t.Fatal("NewBearerAuthFromFile with whitespace-only file: want error, got nil")
	}
}

func TestBearerAuthGRPCAndHTTP(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, []byte("initial-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := apiservice.NewBearerAuthFromFile(tokFile)
	if err != nil {
		t.Fatalf("NewBearerAuthFromFile: %v", err)
	}

	te := newFullTestEnv(t)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(auth.UnaryInterceptor()),
		grpc.StreamInterceptor(auth.StreamInterceptor()),
	)
	ateenvv1alpha.RegisterEnvironmentServiceServer(grpcServer, te.srv)
	ateenvv1alpha.RegisterProcessServiceServer(grpcServer, te.srv)
	ateenvv1alpha.RegisterFileSystemServiceServer(grpcServer, te.srv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	envClient := ateenvv1alpha.NewEnvironmentServiceClient(conn)
	fsClient := ateenvv1alpha.NewFileSystemServiceClient(conn)

	// 1. Unary RPC without bearer token -> Unauthenticated.
	_, err = envClient.CreateEnvironment(context.Background(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "auth-env"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unary without token: got %v, want Unauthenticated", err)
	}

	// 2. Unary RPC with wrong bearer token -> Unauthenticated.
	wrongCtx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer wrong-token")
	_, err = envClient.CreateEnvironment(wrongCtx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "auth-env"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unary with wrong token: got %v, want Unauthenticated", err)
	}

	// 3. Unary RPC with valid bearer token -> succeeds.
	validCtx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer initial-secret")
	if _, err := envClient.CreateEnvironment(validCtx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "auth-env"}); err != nil {
		t.Fatalf("unary with valid token: %v", err)
	}

	// 4. Streaming RPC without bearer token -> Unauthenticated.
	envCtx := metadata.AppendToOutgoingContext(context.Background(), "x-env-id", "auth-env", "x-env-atespace", "ate-env")
	stream, err := fsClient.ReadFile(envCtx, &ateenvv1alpha.ReadFileRequest{Path: "/missing"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("stream without token: got %v, want Unauthenticated", err)
	}

	// 5. Token rotation on disk is picked up without recreating BearerAuth.
	if err := os.WriteFile(tokFile, []byte("rotated-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := envClient.GetEnvironment(validCtx, &ateenvv1alpha.GetEnvironmentRequest{Id: "auth-env"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("old token after rotation: got %v, want Unauthenticated", err)
	}
	rotatedCtx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer rotated-secret")
	if _, err := envClient.GetEnvironment(rotatedCtx, &ateenvv1alpha.GetEnvironmentRequest{Id: "auth-env"}); err != nil {
		t.Fatalf("rotated token: %v", err)
	}

	// 6. HTTP middleware enforces Bearer token while leaving unwrapped /healthz open.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/v1alpha/envs/{id}/mcp", auth.WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "mcp-ok")
	})))
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	res, err := http.Get(httpSrv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", res.StatusCode)
	}

	res, err = http.Post(httpSrv.URL+"/v1alpha/envs/auth-env/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP without token: status = %d, want 401", res.StatusCode)
	}
	if got := res.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}

	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1alpha/envs/auth-env/mcp", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer rotated-secret")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("MCP with valid token: status = %d, want 200", res.StatusCode)
	}
}
