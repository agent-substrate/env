package browser

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/agent-substrate/sandbox/internal/tool"
)

// Config configures the web browsing tool.
type Config struct {
	// Timeout sets the HTTP request timeout. Defaults to 30 seconds.
	Timeout time.Duration

	// MaxBytes caps the number of bytes read from a response body. Defaults to 5MB.
	MaxBytes int64

	// MaxChars caps the number of characters returned in the page content. Defaults to 20000.
	MaxChars int

	// AllowedHosts, if non-empty, restricts fetches to specified hosts/domains.
	AllowedHosts []string

	// BlockedHosts, if non-empty, blocks fetches to specified hosts/domains.
	BlockedHosts []string

	// MaxRedirects caps the number of HTTP redirects followed. Defaults to 5.
	MaxRedirects int

	// Transport overrides the HTTP transport.
	Transport http.RoundTripper
}

func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 5 * 1024 * 1024
	}
	if c.MaxChars <= 0 {
		c.MaxChars = 20000
	}
	if c.MaxRedirects <= 0 {
		c.MaxRedirects = 5
	}
	return c
}

type browseParams struct {
	URL      string `json:"url"`
	Mode     string `json:"mode"`
	MaxChars int    `json:"max_chars"`
}

// New returns the web browsing tool.
func New(cfg Config) tool.Tool {
	cfg = cfg.withDefaults()
	client := cfg.httpClient()

	desc := "Fetch a web page or API response over HTTP(S). mode=markdown (default) converts " +
		"the page to Markdown, preserving headings, lists, tables, and links — link targets " +
		"are absolute, so you can follow them in a later call. mode=raw returns the " +
		"unmodified body. Long pages are truncated. This " +
		"fetches HTML and does not execute JavaScript, so a client-rendered page may return " +
		"little content."
	if len(cfg.AllowedHosts) > 0 {
		desc += " Only these hosts are reachable: " + strings.Join(cfg.AllowedHosts, ", ") + "."
	}

	def := tool.ToolDefinition{
		Name:        "browser",
		Description: desc,
		Parameters: tool.Object([]string{"url"}, map[string]tool.Property{
			"url":       tool.String("Absolute http:// or https:// URL to fetch."),
			"mode":      tool.Enum("How to render the response. Defaults to markdown.", "markdown", "raw"),
			"max_chars": tool.Integer(fmt.Sprintf("Maximum characters to return (default %d).", cfg.MaxChars)),
		}),
	}

	return tool.New(def, func(ctx context.Context, p browseParams) (string, error) {
		target, err := cfg.checkURL(p.URL)
		if err != nil {
			return "", err
		}
		mode := strings.ToLower(strings.TrimSpace(p.Mode))
		if mode == "" {
			mode = "markdown"
		}
		switch mode {
		case "markdown", "raw":
		default:
			return "", fmt.Errorf("unknown mode %q; use markdown or raw", p.Mode)
		}

		ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return "", fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.5")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("fetch %s: %w", target.Redacted(), err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.MaxBytes))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", target.Redacted(), err)
		}

		contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
		final := resp.Request.URL

		var header strings.Builder
		fmt.Fprintf(&header, "%s\n", final.Redacted())
		if final.String() != target.String() {
			fmt.Fprintf(&header, "(redirected from %s)\n", target.Redacted())
		}
		fmt.Fprintf(&header, "status: %s\ncontent-type: %s\nbytes: %d\n", resp.Status, orUnknown(contentType), len(body))

		doc := string(body)
		isHTML := strings.Contains(contentType, "html") ||
			(contentType == "" && strings.Contains(strings.ToLower(doc[:min(len(doc), 512)]), "<html"))

		var rendered string
		switch {
		case mode == "raw":
			rendered = doc
		case isHTML:
			if title := htmlTitle(doc); title != "" {
				fmt.Fprintf(&header, "title: %s\n", title)
			}
			if d := htmlDescription(doc); d != "" {
				fmt.Fprintf(&header, "description: %s\n", d)
			}
			rendered, err = toMarkdown(ctx, doc, final)
			if err != nil {
				return "", fmt.Errorf("convert %s to markdown: %w", final.Redacted(), err)
			}
			if rendered == "" {
				rendered = "(no content extracted — the page may render its content with JavaScript; try mode=raw)"
			}
		case isTextual(contentType):
			rendered = doc
		default:
			return "", fmt.Errorf("%s returned %s (%d bytes), which is not text; this tool only reads text responses",
				final.Redacted(), orUnknown(contentType), len(body))
		}

		page, note := truncate(rendered, clampLimit(p.MaxChars, cfg.MaxChars))
		out := header.String() + "\n" + page
		if note != "" {
			out += "\n\n" + note
		}
		return out, nil
	})
}

func (c Config) httpClient() *http.Client {
	var transport http.RoundTripper = c.Transport
	if transport == nil {
		dialer := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control: func(network, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return fmt.Errorf("unexpected address %q", address)
				}
				ip := net.ParseIP(host)
				if ip == nil {
					return fmt.Errorf("could not parse resolved address %q", host)
				}
				if isInternal(ip) {
					return fmt.Errorf("refusing to connect to internal address %s: "+
						"loopback, link-local, private, and CGNAT ranges are not reachable", ip)
				}
				return nil
			},
		}
		transport = &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: c.Timeout,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       60 * time.Second,
		}
	}
	return &http.Client{
		Timeout:   c.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= c.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", c.MaxRedirects)
			}
			_, err := c.checkURL(req.URL.String())
			return err
		},
	}
}

func (c Config) checkURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("url must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return nil, fmt.Errorf("url %q has no scheme; use an absolute http:// or https:// URL", raw)
	default:
		return nil, fmt.Errorf("scheme %q is not supported; use http or https", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("url %q has no host", raw)
	}
	for _, blocked := range c.BlockedHosts {
		if hostMatches(host, blocked) {
			return nil, fmt.Errorf("host %q is blocked", host)
		}
	}
	if len(c.AllowedHosts) > 0 {
		for _, allowed := range c.AllowedHosts {
			if hostMatches(host, allowed) {
				return u, nil
			}
		}
		return nil, fmt.Errorf("host %q is not in the allowed host list (%s)", host, strings.Join(c.AllowedHosts, ", "))
	}
	return u, nil
}

func hostMatches(host, pattern string) bool {
	pattern = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(pattern), "*."))
	return host == pattern || strings.HasSuffix(host, "."+pattern)
}

func isInternal(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		if v4[0] == 0 {
			return true
		}
	}
	return false
}

func isTextual(contentType string) bool {
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	switch contentType {
	case "application/json", "application/ld+json", "application/xml", "application/xhtml+xml",
		"application/javascript", "application/x-yaml", "application/yaml", "application/rss+xml",
		"application/atom+xml", "":
		return true
	}
	return strings.HasSuffix(contentType, "+json") || strings.HasSuffix(contentType, "+xml")
}

func truncate(s string, limit int) (page, note string) {
	runes := []rune(s)
	if len(runes) <= limit {
		return s, ""
	}
	return string(runes[:limit]), fmt.Sprintf("[truncated: showing first %d of %d characters]", limit, len(runes))
}

func clampLimit(requested, max int) int {
	if requested <= 0 || requested > max {
		return max
	}
	return requested
}

func orUnknown(s string) string {
	if s == "" {
		return "(unspecified)"
	}
	return s
}
