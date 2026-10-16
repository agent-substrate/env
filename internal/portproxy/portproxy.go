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
// /v1alpha/atespaces/{atespace}/envs/{id}/ports/{port}/... . The proxy opens
// an HTTP CONNECT tunnel through the atenet router's CONNECT listener, naming
// the actor in ate-target-actor and the port in the CONNECT authority. The
// router resumes a suspended actor before it answers, then forwards the tunnel
// to that port. The proxy speaks HTTP/1.1 to the daemon through the tunnel and
// keeps idle tunnels for reuse, one pool for each environment and port.
//
// Trust model: the proxy does not authenticate callers. Whoever can reach the
// listener it is served on can reach any TCP port of any actor the router
// serves, in any atespace, including actors that ate-env did not create. A
// daemon behind it must authenticate its own clients. The path form refuses
// requests that a browser sends from another origin (see Options.TrustedOrigins),
// so a web page cannot drive a daemon through a port-forwarded listener.
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

// DefaultWakeBudget bounds how long a request waits for a suspended
// environment to wake. The router's CONNECT is what resumes a suspended
// actor, and the router holds it only for its own parking budget, a few
// seconds; a wake that pulls an image or reads a snapshot across a region
// takes longer. The same two minutes as ate-env-api's guest calls
// (apiservice.DefaultWakeBudget).
const DefaultWakeBudget = 2 * time.Minute

// Idle tunnels: each one is a TCP stream from the API to an actor, held open
// by the router and the worker. Keep few, and not for long, so an idle
// environment is not held awake and a large fleet does not pin thousands
// of sockets in the API pod.
const (
	idleTunnelTimeout  = 10 * time.Second
	idleTunnelsPerPort = 4
	idleTunnelsTotal   = 1024
)

const (
	wakeRetryInitialBackoff = 200 * time.Millisecond
	wakeRetryMaxBackoff     = 2 * time.Second
)

// guestPort is where the ate-env guest listens in every environment. It is
// not a daemon port: the API already serves it, and a daemon cannot bind it.
const guestPort = 80

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
	// ResponseHeaderTimeout bounds the wait for the daemon's response headers
	// after the request is sent. Zero means no limit: a daemon that answers a
	// long command only when it finishes is served, and the caller's own
	// deadline ends a wait on a daemon that never answers.
	ResponseHeaderTimeout time.Duration
	// TrustedOrigins are browser origins (scheme://host[:port]) allowed to use
	// the path form from a page they serve, next to the listener's own origin.
	TrustedOrigins []string
	// WakeBudget bounds how long a request waits while the router answers
	// 503 for an environment that is suspended or still waking. Zero means
	// DefaultWakeBudget.
	WakeBudget time.Duration
}

// Proxy is an http.Handler that forwards each request to the environment port
// its headers name.
type Proxy struct {
	opts Options
	rp   *httputil.ReverseProxy
	tr   *http.Transport
	cop  *http.CrossOriginProtection
}

// New returns a Proxy. It fails only on a malformed trusted origin.
func New(opts Options) (*Proxy, error) {
	if opts.HostSuffix == "" {
		opts.HostSuffix = ate.DefaultHostSuffix
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if opts.WakeBudget <= 0 {
		opts.WakeBudget = DefaultWakeBudget
	}
	p := &Proxy{opts: opts, cop: http.NewCrossOriginProtection()}
	for _, o := range opts.TrustedOrigins {
		if err := p.cop.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("trusted origin: %w", err)
		}
	}
	p.tr = &http.Transport{
		DialContext:           p.dial,
		MaxIdleConnsPerHost:   idleTunnelsPerPort,
		MaxIdleConns:          idleTunnelsTotal,
		IdleConnTimeout:       idleTunnelTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		// Pass encodings through untouched; the daemon and the caller
		// negotiate them.
		DisableCompression: true,
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite:   p.rewrite,
		Transport: p.tr,
		// Flush each write at once, so streamed responses (server-sent events,
		// chunked output) reach the caller as the daemon produces them.
		FlushInterval: -1,
		ErrorHandler:  p.handleError,
	}
	return p, nil
}

// Wants reports whether r uses the header form: it carries EnvPortHeader and
// is not a gRPC call. Callers of the gRPC API never send the header, and the
// tunnel carries HTTP/1.1 only.
// Close drops the idle tunnels. Requests in flight are not affected.
func (p *Proxy) Close() { p.tr.CloseIdleConnections() }

func Wants(r *http.Request) bool {
	return r.Header.Get(EnvPortHeader) != "" &&
		!strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// Dispatch returns a handler that sends header-form requests to p.ServeHTTP,
// path-form requests to p.ServePath, and all others to next. The path form is
// matched here rather than on a ServeMux, so the daemon gets the path exactly
// as sent: a mux would redirect a missing trailing slash, "//" and "..", and
// WebSocket clients do not follow redirects.
func Dispatch(p *Proxy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case Wants(r):
			p.ServeHTTP(w, r)
		case isPathForm(r.URL.EscapedPath()):
			p.ServePath(w, r)
		default:
			next.ServeHTTP(w, r)
		}
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

// canonicalPort is a TCP port in decimal: no sign, no leading zero.
var canonicalPort = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

func parseTarget(r *http.Request, defaultAtespace string) (target, error) {
	for _, h := range []string{EnvIDHeader, EnvAtespaceHeader, EnvPortHeader} {
		if len(r.Header.Values(h)) > 1 {
			return target{}, fmt.Errorf("%s appears more than once", h)
		}
	}
	if r.Header.Get(EnvIDHeader) == "" {
		return target{}, fmt.Errorf("%s is required next to %s", EnvIDHeader, EnvPortHeader)
	}
	return makeTarget(r.Header.Get(EnvIDHeader), r.Header.Get(EnvAtespaceHeader), r.Header.Get(EnvPortHeader), defaultAtespace)
}

// makeTarget validates a target from either form.
func makeTarget(id, atespace, portStr, defaultAtespace string) (target, error) {
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
	n, err := strconv.Atoi(portStr)
	if !canonicalPort.MatchString(portStr) || err != nil || n > 65535 {
		return t, fmt.Errorf("port %q is not a TCP port (1-65535)", portStr)
	}
	if n == guestPort {
		return t, fmt.Errorf("port %d is the ate-env guest, which ate-env-api serves itself; a daemon listens on another port", guestPort)
	}
	t.port = n
	return t, nil
}

// pathPrefix splits an escaped path of the path form into its routing
// segments and the rest. n is 0 when the path does not have that shape. The
// split is by segment on the escaped path, the way a ServeMux matches, so an
// escaped routing segment (%65nv1 for env1) still ends the prefix where it
// should.
func pathPrefix(escaped string) (id, atespace, portStr, rest string, n int) {
	segs := strings.Split(escaped, "/")
	switch {
	case len(segs) >= 6 && segs[0] == "" && segs[1] == "v1alpha" && segs[2] == "envs" && segs[4] == "ports":
		id, portStr, n = segs[3], segs[5], 6
	case len(segs) >= 8 && segs[0] == "" && segs[1] == "v1alpha" && segs[2] == "atespaces" && segs[4] == "envs" && segs[6] == "ports":
		atespace, id, portStr, n = segs[3], segs[5], segs[7], 8
	default:
		return "", "", "", "", 0
	}
	return id, atespace, portStr, "/" + strings.Join(segs[n:], "/"), n
}

func isPathForm(escaped string) bool {
	_, _, _, _, n := pathPrefix(escaped)
	return n > 0
}

func parsePath(escaped, defaultAtespace string) (target, error) {
	id, atespace, portStr, rest, n := pathPrefix(escaped)
	if n == 0 {
		return target{}, errors.New("path is not /v1alpha/[atespaces/{atespace}/]envs/{id}/ports/{port}[/rest]")
	}
	// Validation admits only labels and digits, thus an escaped routing
	// segment is unescaped first and the value that passes reads the same both
	// ways.
	for _, s := range []*string{&id, &atespace, &portStr} {
		v, err := url.PathUnescape(*s)
		if err != nil {
			return target{}, fmt.Errorf("bad path escaping: %w", err)
		}
		*s = v
	}
	t, err := makeTarget(id, atespace, portStr, defaultAtespace)
	if err != nil {
		return t, err
	}
	// The rest keeps its own escaping. The bare prefix, with or without a
	// trailing slash, is the daemon's root.
	t.rawPath = rest
	if t.path, err = url.PathUnescape(rest); err != nil {
		return t, fmt.Errorf("bad path escaping: %w", err)
	}
	if t.rawPath == t.path {
		t.rawPath = ""
	}
	return t, nil
}

// ServePath forwards a path-form request to the environment port its path
// names, after it strips the routing prefix. It refuses a request that a
// browser sends from another origin: a state-changing method, or a WebSocket
// handshake, which is a GET that opens a channel both ways. The header form
// needs no such check, because a browser sends custom headers to another
// origin only after a CORS preflight, which the proxy never approves.
func (p *Proxy) ServePath(w http.ResponseWriter, r *http.Request) {
	t, err := parsePath(r.URL.EscapedPath(), p.opts.DefaultAtespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	check := r
	if r.Header.Get("Upgrade") != "" {
		// CrossOriginProtection lets safe methods through; judge a handshake
		// as the state-changing request it is.
		check = r.WithContext(r.Context())
		check.Method = http.MethodPost
	}
	if err := p.cop.Check(check); err != nil {
		http.Error(w, "cross-origin request to an environment port refused; "+
			"add the page's origin to the port proxy's trusted origins", http.StatusForbidden)
		return
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
// rewrite). While the router answers 503, which is what a suspended actor
// and one still waking both look like, it tries again with backoff until
// WakeBudget is spent: the first CONNECT is what resumes the actor, and the
// router holds it only for its own parking budget.
func (p *Proxy) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	id, atespace, _ := strings.Cut(host, ".")
	deadline := time.Now().Add(p.opts.WakeBudget)
	backoff := wakeRetryInitialBackoff
	for attempt := 1; ; attempt++ {
		conn, err := p.connect(ctx, id, atespace, portStr)
		var re *routerError
		if !errors.As(err, &re) || re.status != http.StatusServiceUnavailable {
			return conn, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Logged here: handleError keeps router 503s out of the log, and
			// a spent wake budget is the one 503 an operator wants to see,
			// as the close of the "waiting" line from the first attempt.
			err = fmt.Errorf("environment %s/%s did not wake within %s (%d attempts): %w", atespace, id, p.opts.WakeBudget, attempt, err)
			log.Printf("port proxy: %v", err)
			return nil, err
		}
		if attempt == 1 {
			log.Printf("port proxy: waiting for environment %s/%s to wake (wake budget %s)", atespace, id, p.opts.WakeBudget)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(backoff, remaining)):
		}
		backoff = min(backoff*2, wakeRetryMaxBackoff)
	}
}

// connect sends one CONNECT for the environment port and returns the tunnel.
func (p *Proxy) connect(ctx context.Context, id, atespace, portStr string) (net.Conn, error) {
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

// CloseWrite half-closes the tunnel when the connection can: after an
// upgrade, httputil.ReverseProxy half-closes the backend once the client
// side is done, and embedding net.Conn would otherwise hide the method.
func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// handleError answers a failed forward. The body names the target and a
// short cause; the full error, which can name the router's address, goes to
// the log only.
func (p *Proxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	t, _ := r.Context().Value(targetKey{}).(target)
	var re *routerError
	var ne net.Error
	status, cause := http.StatusBadGateway, "is unreachable; the ate-env-api log has the cause"
	switch {
	case errors.Is(err, context.Canceled):
		return // the caller went away; nobody reads an answer
	case errors.As(err, &re):
		// 404 (no such environment) and 503 (not running) mean the same to
		// the caller as to the router; anything else is a gateway failure.
		switch re.status {
		case http.StatusNotFound:
			status, cause = re.status, "was not found"
		case http.StatusServiceUnavailable:
			status, cause = re.status, "is not available (not running, or still waking)"
		case http.StatusBadGateway:
			cause = "refused the connection; a daemon must listen on 0.0.0.0, not only on 127.0.0.1"
		}
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		status, cause = http.StatusGatewayTimeout, "did not answer in time"
	}
	// A 404 or 503 from the router is the caller's answer, not an operator's
	// problem; a log line for each would flood the log when a fleet wakes.
	if status != http.StatusNotFound && status != http.StatusServiceUnavailable {
		log.Printf("port proxy: %s/%s port %d: %v", t.atespace, t.id, t.port, err)
	}
	http.Error(w, fmt.Sprintf("environment %s/%s port %d %s", t.atespace, t.id, t.port, cause), status)
}
