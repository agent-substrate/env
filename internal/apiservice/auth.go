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

package apiservice

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// BearerAuth authenticates incoming gRPC and HTTP callers against an expected
// bearer token, loaded either from a static string or re-read on each check
// from a token file so rotated Secret mounts take effect without a restart.
type BearerAuth struct {
	token     string
	tokenFile string
}

// NewBearerAuth creates a BearerAuth that requires the given static token.
func NewBearerAuth(token string) (*BearerAuth, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("apiservice: bearer token must not be empty")
	}
	return &BearerAuth{token: token}, nil
}

// NewBearerAuthFromFile creates a BearerAuth backed by path, validating at
// construction time that path is readable and non-empty.
func NewBearerAuthFromFile(path string) (*BearerAuth, error) {
	if path == "" {
		return nil, errors.New("apiservice: bearer token file path is required")
	}
	auth := &BearerAuth{tokenFile: path}
	if _, err := auth.expectedToken(); err != nil {
		return nil, err
	}
	return auth, nil
}

func (a *BearerAuth) expectedToken() (string, error) {
	if a.tokenFile == "" {
		return a.token, nil
	}
	b, err := os.ReadFile(a.tokenFile)
	if err != nil {
		return "", fmt.Errorf("apiservice: reading bearer token file %q: %w", a.tokenFile, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("apiservice: bearer token file %q is empty", a.tokenFile)
	}
	return tok, nil
}

func (a *BearerAuth) checkHeader(authHeader string) error {
	scheme, got, ok := strings.Cut(strings.TrimSpace(authHeader), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	got = strings.TrimSpace(got)
	if got == "" {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	want, err := a.expectedToken()
	if err != nil {
		return status.Errorf(codes.Internal, "verifying bearer token: %v", err)
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	return nil
}

func (a *BearerAuth) authorizeContext(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	return a.checkHeader(vals[0])
}

// UnaryInterceptor returns a gRPC UnaryServerInterceptor that enforces
// Authorization: Bearer <token>.
func (a *BearerAuth) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := a.authorizeContext(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor returns a gRPC StreamServerInterceptor that enforces
// Authorization: Bearer <token>.
func (a *BearerAuth) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := a.authorizeContext(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// WrapHTTP wraps next with HTTP middleware that enforces Authorization: Bearer <token>.
func (a *BearerAuth) WrapHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.checkHeader(r.Header.Get("Authorization")); err != nil {
			if status.Code(err) == codes.Internal {
				http.Error(w, status.Convert(err).Message(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
