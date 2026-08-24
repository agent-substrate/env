// Package fakerouter implements a test double for the atenet router: it
// resolves the target environment from the request's Host header and forwards
// to that environment's guest handler, returning 503 when the environment is not
// running. Like the real router, it carries the downstream protocol through:
// it accepts cleartext HTTP/2, so gRPC traffic reaches the guest as h2c.
package fakerouter

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/grpcmux"
)

// Router is a fake atenet router.
type Router struct {
	// Running reports whether the actor with the given ID is currently
	// running. Non-running actors get a 503.
	Running func(id string) bool

	mu     sync.Mutex
	guests map[string]http.Handler
}

// New returns a Router that considers every actor running unless a Running
// callback is set.
func New() *Router {
	return &Router{guests: make(map[string]http.Handler)}
}

// Register installs the guest handler serving a environment ID.
func (r *Router) Register(id string, h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guests[id] = h
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id, _, ok := strings.Cut(req.Host, ".")
	if !ok || !strings.HasSuffix(req.Host, "."+ate.DefaultHostSuffix) {
		http.Error(w, "unroutable host "+req.Host, http.StatusNotFound)
		return
	}
	r.mu.Lock()
	guest := r.guests[id]
	r.mu.Unlock()
	if guest == nil {
		http.Error(w, "no actor "+id, http.StatusNotFound)
		return
	}
	if r.Running != nil && !r.Running(id) {
		http.Error(w, "actor "+id+" is not running", http.StatusServiceUnavailable)
		return
	}
	guest.ServeHTTP(w, req)
}

// Serve starts the router on a random localhost port and returns its
// address and a shutdown function.
func (r *Router) Serve() (addr string, stop func()) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("fakerouter: " + err.Error())
	}
	srv := grpcmux.Server(r)
	go srv.Serve(lis)
	return lis.Addr().String(), func() { srv.Close() }
}

// ServeTLS starts the router on a random localhost port serving HTTPS
// with a self-signed certificate and h2 offered via ALPN — the shape of
// an atenet router running with --https-h2. Clients must skip
// certificate verification.
func (r *Router) ServeTLS() (addr string, stop func()) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("fakerouter: " + err.Error())
	}
	cert, err := selfSignedCert()
	if err != nil {
		panic("fakerouter: " + err.Error())
	}
	srv := grpcmux.Server(r)
	srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	go srv.ServeTLS(lis, "", "")
	return lis.Addr().String(), func() { srv.Close() }
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fakerouter"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
