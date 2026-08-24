package grpcproxy_test

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/grpcmux"
	"github.com/agent-substrate/env/internal/grpcproxy"
	"github.com/agent-substrate/env/internal/guest/guestrpc"
	"github.com/agent-substrate/env/internal/guest/guestsys"
	"github.com/agent-substrate/env/internal/guest/proc"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	pb "github.com/agent-substrate/env/proto/ateenv/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestProxyOverRouterTLS runs the data plane against a router that only
// speaks HTTPS with h2 offered via ALPN — the atenet --https-h2 shape —
// with the client dialing it via Options.RouterTLS.
func TestProxyOverRouterTLS(t *testing.T) {
	t.Chdir(t.TempDir())

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

	guestGrpc := grpc.NewServer()
	guestrpc.Register(guestGrpc, guestsys.New(), proc.NewTable())
	router.Register("env-tls", grpcmux.Handler(guestGrpc, http.NotFoundHandler()))
	t.Cleanup(guestGrpc.Stop)
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
			func(ctx context.Context, envID string) (*grpc.ClientConn, error) {
				return client.GuestConn(envID)
			},
		)),
	)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpcmux.Server(grpcmux.Handler(proxyServer, http.NotFoundHandler()))
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Close() })

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	res, err := pb.NewProcessClient(conn).Exec(envCtx(t.Context(), "env-tls"), &pb.ExecRequest{
		Command: "printf over-tls",
	})
	if err != nil {
		t.Fatalf("Exec through TLS router: %v", err)
	}
	if got := string(res.GetStdout()); got != "over-tls" {
		t.Fatalf("Exec = %q, want over-tls", got)
	}
}
