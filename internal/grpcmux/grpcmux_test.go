package grpcmux_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/grpcmux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// startServer starts a grpcmux server on 127.0.0.1:0 with the standard
// gRPC health service on the gRPC plane and a small REST mux on the
// fallback plane, and returns its host:port. The REST mux serves 200
// "ok" on GET /healthz and echoes r.Proto on GET /proto.
func startServer(t *testing.T) string {
	t.Helper()

	grpcServer := grpc.NewServer()
	healthpb.RegisterHealthServer(grpcServer, health.NewServer())

	rest := http.NewServeMux()
	rest.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	rest.HandleFunc("GET /proto", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	})

	srv := grpcmux.Server(grpcmux.Handler(grpcServer, rest))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		srv.Close()
		<-done
	})
	return ln.Addr().String()
}

// http1Client returns a client that speaks plain HTTP/1.1.
func http1Client(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// h2cClient returns a client that speaks prior-knowledge unencrypted
// HTTP/2 for http:// URLs, using only the standard library.
func h2cClient(t *testing.T) *http.Client {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: protocols}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// healthClient returns a gRPC health client connected over cleartext
// (prior-knowledge h2c) to addr.
func healthClient(t *testing.T, addr string) healthpb.HealthClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%q): %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return healthpb.NewHealthClient(conn)
}

// get performs a GET and returns the response (with body read and
// closed) and the body text.
func get(t *testing.T, client *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: reading body: %v", url, err)
	}
	return resp, string(body)
}

// checkHealth polls the health service until it reports SERVING or ctx
// expires. WaitForReady covers RPCs waiting for a ready transport; the
// loop additionally absorbs the transient Unavailable an RPC gets when
// its transport breaks after dispatch, which says nothing about the
// mux. A real routing defect would fail persistently and exhaust ctx.
func checkHealth(ctx context.Context, client healthpb.HealthClient) error {
	for {
		resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true))
		switch {
		case err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING:
			return nil
		case err == nil:
			err = errors.New("health status = " + resp.GetStatus().String() + ", want SERVING")
			fallthrough
		case status.Code(err) == codes.Unavailable:
			select {
			case <-ctx.Done():
				return err
			case <-time.After(50 * time.Millisecond):
			}
		default:
			return err
		}
	}
}

func TestRESTOverHTTP1(t *testing.T) {
	addr := startServer(t)

	resp, body := get(t, http1Client(t), "http://"+addr+"/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if body != "ok" {
		t.Errorf("GET /healthz body = %q, want %q", body, "ok")
	}
	if resp.ProtoMajor != 1 {
		t.Errorf("GET /healthz proto = %s, want HTTP/1.x", resp.Proto)
	}
}

func TestGRPCHealthCheck(t *testing.T) {
	addr := startServer(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := checkHealth(ctx, healthClient(t, addr)); err != nil {
		t.Fatalf("health check over shared port: %v", err)
	}
}

func TestRESTOverUnencryptedHTTP2(t *testing.T) {
	addr := startServer(t)

	resp, body := get(t, h2cClient(t), "http://"+addr+"/proto")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /proto status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	// The REST handler echoes r.Proto: "HTTP/2.0" proves the request
	// reached it with r.ProtoMajor == 2 rather than being downgraded
	// or swallowed by the gRPC plane.
	if body != "HTTP/2.0" {
		t.Errorf("REST handler saw proto %q, want %q", body, "HTTP/2.0")
	}
	if resp.ProtoMajor != 2 {
		t.Errorf("response proto = %s, want HTTP/2.x", resp.Proto)
	}
}

func TestInterleavedGRPCAndREST(t *testing.T) {
	addr := startServer(t)

	hc := healthClient(t, addr)
	h1 := http1Client(t)
	h2 := h2cClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Alternate planes sequentially on the one live server.
	for i := 0; i < 5; i++ {
		if err := checkHealth(ctx, hc); err != nil {
			t.Fatalf("iteration %d: gRPC health check: %v", i, err)
		}
		if resp, body := get(t, h1, "http://"+addr+"/healthz"); resp.StatusCode != http.StatusOK || body != "ok" {
			t.Fatalf("iteration %d: HTTP/1.1 GET /healthz = %d %q, want 200 %q", i, resp.StatusCode, body, "ok")
		}
		if resp, body := get(t, h2, "http://"+addr+"/proto"); resp.StatusCode != http.StatusOK || body != "HTTP/2.0" {
			t.Fatalf("iteration %d: h2c GET /proto = %d %q, want 200 %q", i, resp.StatusCode, body, "HTTP/2.0")
		}
	}

	// And concurrently: both planes hammered at once must keep working.
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if err := checkHealth(ctx, hc); err != nil {
					t.Errorf("concurrent gRPC health check: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				resp, err := h1.Get("http://" + addr + "/healthz")
				if err != nil {
					t.Errorf("concurrent GET /healthz: %v", err)
					return
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Errorf("concurrent GET /healthz: reading body: %v", err)
					return
				}
				if resp.StatusCode != http.StatusOK || string(body) != "ok" {
					t.Errorf("concurrent GET /healthz = %d %q, want 200 %q", resp.StatusCode, body, "ok")
					return
				}
			}
		}()
	}
	wg.Wait()
}
