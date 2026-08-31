package idle

import (
	"context"
	"strings"

	"github.com/agent-substrate/env/internal/ate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// UnaryServerInterceptor records activity on the environment named by a
// unary RPC, pinning it while the call is in flight. A successful manual
// SuspendEnvironment or DeleteEnvironment drops the environment from the
// tracker instead.
func UnaryServerInterceptor(t *Tracker) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		env, ok := envFromContext(ctx)
		if !ok {
			env, ok = envFromRequest(req)
		}
		if !ok {
			return handler(ctx, req)
		}
		release := t.Pin(env)
		resp, err := handler(ctx, req)
		release()
		if err == nil && (strings.HasSuffix(info.FullMethod, "/SuspendEnvironment") ||
			strings.HasSuffix(info.FullMethod, "/DeleteEnvironment")) {
			t.Forget(env)
		}
		return resp, err
	}
}

// StreamServerInterceptor pins the environment named by a streaming RPC for
// the lifetime of the stream, so a held output stream or a file transfer is
// never suspended under the caller.
func StreamServerInterceptor(t *Tracker) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		env, ok := envFromContext(ss.Context())
		if !ok {
			return handler(srv, ss)
		}
		defer t.Pin(env)()
		return handler(srv, ss)
	}
}

// envFromContext extracts the target environment from x-env-id and
// x-env-atespace request metadata, as sent to the proxied ProcessService and
// FileSystemService.
func envFromContext(ctx context.Context) (Env, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return Env{}, false
	}
	ids := md.Get("x-env-id")
	if len(ids) == 0 || ids[0] == "" {
		return Env{}, false
	}
	env := Env{Atespace: ate.DefaultAtespace, ID: ids[0]}
	if atespaces := md.Get("x-env-atespace"); len(atespaces) > 0 && atespaces[0] != "" {
		env.Atespace = atespaces[0]
	}
	return env, true
}

// envFromRequest extracts the target environment from an EnvironmentService
// request message.
func envFromRequest(req any) (Env, bool) {
	m, ok := req.(interface{ GetId() string })
	if !ok || m.GetId() == "" {
		return Env{}, false
	}
	env := Env{Atespace: ate.DefaultAtespace, ID: m.GetId()}
	if a, ok := req.(interface{ GetAtespace() string }); ok && a.GetAtespace() != "" {
		env.Atespace = a.GetAtespace()
	}
	return env, true
}
