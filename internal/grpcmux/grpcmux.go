// Package grpcmux serves gRPC and a plain HTTP API on one port. gRPC
// arrives as HTTP/2 — cleartext (h2c, via prior knowledge) or TLS —
// and is told apart from other traffic by its content type; everything
// else goes to the fallback handler. The h2c support is the standard
// library's (http.Protocols), so no extra dependency is needed.
package grpcmux

import (
	"net/http"
	"strings"

	"google.golang.org/grpc"
)

// Handler routes application/grpc requests to grpcServer and everything
// else to rest.
func Handler(grpcServer *grpc.Server, rest http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		rest.ServeHTTP(w, r)
	})
}

// Server returns an http.Server that serves handler over HTTP/1.1 and
// HTTP/2, including cleartext HTTP/2, on one port. Wrap the handlers
// with Handler to share the port between gRPC and REST.
func Server(handler http.Handler) *http.Server {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:   handler,
		Protocols: protocols,
	}
}
