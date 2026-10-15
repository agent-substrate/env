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

// Package portproxy forwards HTTP requests from callers outside the actor
// network to a TCP port inside an environment, such as a daemon that a runtime
// layer starts next to the guest (docs/rootfs-images/RUNTIMES.md).
//
// A request names its target with three headers, EnvIDHeader,
// EnvAtespaceHeader (optional) and EnvPortHeader, or, for clients that cannot
// set headers (WebSocket clients, browsers), with a path prefix that the proxy
// strips: /v1alpha/envs/{id}/ports/{port}/... in the default atespace, or
// /v1alpha/atespaces/{atespace}/envs/{id}/ports/{port}/... . The proxy opens an HTTP CONNECT tunnel through
// the atenet router's CONNECT listener, naming the actor in ate-target-actor
// and the port in the CONNECT authority. The router resumes a suspended actor
// before it answers, then forwards the tunnel to that port. The proxy speaks
// HTTP/1.1 to the daemon through the tunnel and keeps idle tunnels for reuse,
// one pool for each environment and port.
package portproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agent-substrate/env/internal/ate"
)

// The routing headers. They match the x-env-id and x-env-atespace metadata
// keys of the gRPC API, so a caller uses one vocabulary for both.
const (
	EnvIDHeader       = "X-Env-Id"
	EnvAtespaceHeader = "X-Env-Atespace"
	EnvPortHeader     = "X-Env-Port"
)

// DefaultHandshakeTimeout bounds dialing the router and waiting for its
// answer to CONNECT, which includes resuming a suspended actor.
const DefaultHandshakeTimeout = 2 * time.Minute

// Options configures a Proxy.
type Options struct {
	// ConnectAddr is the router's plaintext CONNECT listener, host:port.
	ConnectAddr string
	// HostSuffix is the router's actor host suffix; the CONNECT authority is
	// <id>.<suffix>:<port>. The router reads only the port from it.
	HostSuffix string
	// DefaultAtespace applies when a request carries no EnvAtespaceHeader.
	DefaultAtespace string
	// HandshakeTimeout bounds the CONNECT handshake. Zero means
	// DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
}

// Proxy is an http.Handler that forwards each request to the environment port
// its headers name.
type Proxy struct {
	opts Options
	rp   *httputil.ReverseProxy
}

// New returns a Proxy.
func New(opts Options) *Proxy {
	if opts.HostSuffix == "" {
		opts.HostSuffix = ate.DefaultHostSuffix
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = DefaultHandshakeTimeout
	}
	p := &Proxy{opts: opts}
	p.rp = &httputil.ReverseProxy{
		Rewrite: p.rewrite,
		Transport: &http.Transport{
			DialContext:         p.dial,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			// Pass encodings through untouched; the daemon and the caller
			// negotiate them.
			DisableCompression: true,
		},
		// Flush each write at once, so streamed responses (server-sent events,
		// chunked output) reach the caller as the daemon produces them.
		FlushInterval: -1,
		ErrorHandler:  p.handleError,
	}
	return p
}

// Wants reports whether r is addressed to the proxy: it carries
// EnvPortHeader. Callers of the gRPC API never send it.
func Wants(r *http.Request) bool {
	return r.Header.Get(EnvPortHeader) != ""
}

// Dispatch returns a handler that sends requests the proxy Wants to p and all
// others to next.
func Dispatch(p *Proxy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Wants(r) {
			p.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type target struct {
	atespace string
	id       string
	port     int
	// path and rawPath, when set, replace the request's path upstream (the
	// path form strips its routing prefix).
	path, rawPath string
}

type targetKey struct{}

// label is a DNS-1123 label, the shape of environment ids and atespaces. The
// values go into a CONNECT request line and header, so nothing else may pass.
var label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func parseTarget(r *http.Request, defaultAtespace string) (target, error) {
	if r.Header.Get(EnvIDHeader) == "" {
		return target{}, fmt.Errorf("%s is required next to %s", EnvIDHeader, EnvPortHeader)
	}
	return makeTarget(r.Header.Get(EnvIDHeader), r.Header.Get(EnvAtespaceHeader), r.Header.Get(EnvPortHeader), defaultAtespace)
}

// makeTarget validates a target from either form.
func makeTarget(id, atespace, port, defaultAtespace string) (target, error) {
	t := target{id: id, atespace: atespace}
	if !label.MatchString(t.id) {
		return t, fmt.Errorf("environment id %q is not valid (lowercase alphanumerics and '-', at most 63)", t.id)
	}
	if t.atespace == "" {
		t.atespace = defaultAtespace
	}
	if !label.MatchString(t.atespace) {
		return t, fmt.Errorf("atespace %q is not valid", t.atespace)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return t, fmt.Errorf("port %q is not a TCP port (1-65535)", port)
	}
	t.port = n
	return t, nil
}

// Register adds the path form of the proxy to mux.
func (p *Proxy) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1alpha/envs/{id}/ports/{port}/{rest...}", p.servePath)
	mux.HandleFunc("/v1alpha/atespaces/{atespace}/envs/{id}/ports/{port}/{rest...}", p.servePath)
}

func (p *Proxy) servePath(w http.ResponseWriter, r *http.Request) {
	t, err := makeTarget(r.PathValue("id"), r.PathValue("atespace"), r.PathValue("port"), p.opts.DefaultAtespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prefix := "/v1alpha/envs/" + r.PathValue("id") + "/ports/" + r.PathValue("port")
	if a := r.PathValue("atespace"); a != "" {
		prefix = "/v1alpha/atespaces/" + a + prefix[len("/v1alpha"):]
	}
	// The prefix is made of validated labels and digits, so it reads the same
	// escaped and unescaped; what follows keeps its own escaping.
	t.rawPath = strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if t.path, err = url.PathUnescape(t.rawPath); err != nil {
		http.Error(w, "bad path escaping: "+err.Error(), http.StatusBadRequest)
		return
	}
	if t.rawPath == t.path {
		t.rawPath = ""
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, t)))
}

// ServeHTTP forwards r to the environment port its headers name.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t, err := parseTarget(r, p.opts.DefaultAtespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, t)))
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	t := pr.In.Context().Value(targetKey{}).(target)
	pr.SetXForwarded()
	// The URL host is the transport's pool key, not an address: one pool for
	// each environment and port. dial reads the target back from it.
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = net.JoinHostPort(t.id+"."+t.atespace, strconv.Itoa(t.port))
	// What a client inside the environment would send.
	pr.Out.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(t.port))
	if t.path != "" {
		pr.Out.URL.Path, pr.Out.URL.RawPath = t.path, t.rawPath
	}
	for _, h := range []string{EnvIDHeader, EnvAtespaceHeader, EnvPortHeader} {
		pr.Out.Header.Del(h)
	}
}

// routerError is a CONNECT the router refused.
type routerError struct {
	status int
	msg    string
}

func (e *routerError) Error() string {
	return fmt.Sprintf("router answered CONNECT with %d: %s", e.status, e.msg)
}

// dial opens a CONNECT tunnel to the environment port encoded in addr (see
// rewrite).
func (p *Proxy) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	id, atespace, _ := strings.Cut(host, ".")

	ctx, cancel := context.WithTimeout(ctx, p.opts.HandshakeTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", p.opts.ConnectAddr)
	if err != nil {
		return nil, fmt.Errorf("dialing the router's CONNECT listener %s: %w", p.opts.ConnectAddr, err)
	}
	// A context that ends mid-handshake unblocks the write or read below.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })

	authority := net.JoinHostPort(id+"."+p.opts.HostSuffix, portStr)
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: authority},
		Host:   authority,
		Header: http.Header{ate.TargetActorHeader: {atespace + "/" + id}},
	}
	br := bufio.NewReader(conn)
	resp, err := func() (*http.Response, error) {
		if err := req.Write(conn); err != nil {
			return nil, err
		}
		return http.ReadResponse(br, req)
	}()
	if !stop() {
		err = errors.Join(ctx.Err(), err)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("CONNECT to %s/%s port %s: %w", atespace, id, portStr, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		conn.Close()
		return nil, &routerError{status: resp.StatusCode, msg: strings.TrimSpace(string(body))}
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// bufferedConn reads what the handshake reader buffered past the CONNECT
// response before it reads the connection again.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (p *Proxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	t, _ := r.Context().Value(targetKey{}).(target)
	var re *routerError
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, context.Canceled):
		return // the caller went away; nobody reads an answer
	case errors.As(err, &re):
		// 404 (no such environment) and 503 (not running) mean the same to
		// the caller as to the router; anything else is a gateway failure.
		if re.status == http.StatusNotFound || re.status == http.StatusServiceUnavailable {
			status = re.status
		}
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	}
	log.Printf("port proxy: %s/%s port %d: %v", t.atespace, t.id, t.port, err)
	http.Error(w, fmt.Sprintf("environment %s/%s port %d: %v", t.atespace, t.id, t.port, err), status)
}
