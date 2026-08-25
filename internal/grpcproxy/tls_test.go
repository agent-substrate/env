package grpcproxy_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/grpcproxy"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestProxyOverRouterTLS runs the data plane against a router that only
// speaks HTTPS with h2 offered via ALPN — the atenet --https-h2 shape —
// with the client dialing it via Options.RouterTLS.
func TestProxyOverRouterTLS(t *testing.T) {
	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	routerAddr, stopRouter := router.ServeTLS()
	t.Cleanup(stopRouter)

	client, err := ate.New(ate.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		RouterTLS:   true,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	guestGrpc, cleanup, err := guest.NewServer(guest.Config{
		LogDir:           t.TempDir(),
		Workspace:        t.TempDir(),
		EnableProcess:    true,
		EnableFileSystem: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(guestGrpc.Stop)
	router.Register("env-tls", guestGrpc)
	if err := client.Create(t.Context(), ate.CreateOptions{
		ID:        "env-tls",
		Template:  "default-env",
		Namespace: "envs",
	}); err != nil {
		t.Fatal(err)
	}

	proxyServer := grpc.NewServer(
		grpc.ForceServerCodec(grpcproxy.Codec()),
		grpc.UnknownServiceHandler(grpcproxy.StreamHandler(
			func(ctx context.Context, atespace, envID string) (*grpc.ClientConn, error) {
				return client.GuestConn(atespace, envID)
			},
		)),
	)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go proxyServer.Serve(lis)
	t.Cleanup(proxyServer.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	out := shell(t, envCtx(t.Context(), "env-tls"), conn, "printf over-tls")
	if !strings.Contains(out, "over-tls") {
		t.Fatalf("shell through TLS router = %q, want over-tls", out)
	}
}
