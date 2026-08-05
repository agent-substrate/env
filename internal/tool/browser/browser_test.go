package browser

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/sandbox/internal/tool"
)

const samplePage = `<!doctype html>
<html>
<head>
  <title>Sample
  Page</title>
  <meta name="description" content="A page for tests.">
  <style>body { color: red }</style>
  <script>console.log("ignored")</script>
</head>
<body>
  <nav><a href="/other">Other page</a></nav>
  <h1>Heading</h1>
  <p>Some <strong>bold</strong> text with an <a href="/relative">absolute-ised link</a>.</p>
  <ul><li>first</li><li>second</li></ul>
  <table>
    <tr><th>Name</th><th>Size</th></tr>
    <tr><td>alpha</td><td>10</td></tr>
  </table>
</body>
</html>`

func browserFixture(t *testing.T) (*tool.Registry, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/binary":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{0x0, 0x1, 0x2})
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(samplePage))
		}
	}))
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	if err := reg.Register(New(Config{Transport: srv.Client().Transport})); err != nil {
		t.Fatal(err)
	}
	return reg, srv
}

func call(t *testing.T, reg *tool.Registry, toolName string, input map[string]any) string {
	t.Helper()
	res := invoke(t, reg, toolName, input)
	if res.IsError {
		t.Fatalf("%s tool returned unexpected error: %s", toolName, res.Content[0].Text)
	}
	return res.Content[0].Text
}

func invoke(t *testing.T, reg *tool.Registry, toolName string, input map[string]any) tool.ToolResult {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return reg.Invoke(context.Background(), tool.ToolUse{
		Type:  tool.BlockTypeToolUse,
		ID:    "test_call",
		Name:  toolName,
		Input: raw,
	})
}

func TestBrowserMarkdown(t *testing.T) {
	reg, srv := browserFixture(t)
	out := call(t, reg, "browser", map[string]any{"url": srv.URL + "/page"})

	for _, want := range []string{
		"status: 200 OK",
		"title: Sample Page",
		"description: A page",
		"# Heading",
		"**bold**",
		"- first",
		"| Name",
		"| alpha",
		srv.URL + "/relative",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"<h1>", "console.log", "color: red"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("markdown output still contains %q:\n%s", unwanted, out)
		}
	}
}

func TestBrowserRawMode(t *testing.T) {
	reg, srv := browserFixture(t)
	out := call(t, reg, "browser", map[string]any{"url": srv.URL + "/page", "mode": "raw"})
	if !strings.Contains(out, "<h1>Heading</h1>") {
		t.Errorf("raw mode did not return the source HTML:\n%s", out)
	}
}

func TestBrowserRejectsRemovedModes(t *testing.T) {
	reg, srv := browserFixture(t)
	for _, mode := range []string{"text", "links"} {
		res := invoke(t, reg, "browser", map[string]any{"url": srv.URL + "/page", "mode": mode})
		if !res.IsError || !strings.Contains(res.Content[0].Text, "use markdown or raw") {
			t.Errorf("mode=%s should be rejected with guidance: %+v", mode, res)
		}
	}
}

func TestBrowserPassesThroughTextResponses(t *testing.T) {
	reg, srv := browserFixture(t)
	out := call(t, reg, "browser", map[string]any{"url": srv.URL + "/json"})
	if !strings.Contains(out, `{"ok":true}`) {
		t.Errorf("JSON body not returned unchanged:\n%s", out)
	}
}

func TestBrowserRejectsBinaryResponses(t *testing.T) {
	reg, srv := browserFixture(t)
	res := invoke(t, reg, "browser", map[string]any{"url": srv.URL + "/binary"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not text") {
		t.Errorf("binary response not rejected: %+v", res)
	}
}

func TestBrowserTruncation(t *testing.T) {
	reg, srv := browserFixture(t)
	out := call(t, reg, "browser", map[string]any{"url": srv.URL + "/page", "max_chars": 40})
	if !strings.Contains(out, "[truncated: showing first 40 of ") {
		t.Errorf("truncated page missing truncation note:\n%s", out)
	}
}

func TestBrowserBlocksInternalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	if err := reg.Register(New(Config{})); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{
		srv.URL,
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
		"http://[::1]:80/",
		"http://2130706433/",
	} {
		res := invoke(t, reg, "browser", map[string]any{"url": url})
		if !res.IsError {
			t.Errorf("fetching %s was allowed: %+v", url, res)
		}
	}
}

func TestBrowserURLValidation(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(New(Config{AllowedHosts: []string{"example.com"}})); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"file:///etc/passwd": "not supported",
		"/just/a/path":       "no scheme",
		"https://evil.test/": "not in the allowed host list",
		"":                   "must not be empty",
	}
	for url, wantErr := range cases {
		res := invoke(t, reg, "browser", map[string]any{"url": url})
		if !res.IsError || !strings.Contains(res.Content[0].Text, wantErr) {
			t.Errorf("url %q: got %+v, want error containing %q", url, res, wantErr)
		}
	}
}

func TestHostMatches(t *testing.T) {
	cases := []struct {
		host, pattern string
		want          bool
	}{
		{"example.com", "example.com", true},
		{"docs.example.com", "example.com", true},
		{"docs.example.com", "*.example.com", true},
		{"notexample.com", "example.com", false},
		{"example.com.evil.test", "example.com", false},
	}
	for _, c := range cases {
		if got := hostMatches(c.host, c.pattern); got != c.want {
			t.Errorf("hostMatches(%q, %q) = %v, want %v", c.host, c.pattern, got, c.want)
		}
	}
}

func TestIsInternal(t *testing.T) {
	internal := []string{"127.0.0.1", "::1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", "100.100.0.1", "0.0.0.0", "fd00::1"}
	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111", "99.99.99.99"}

	for _, ip := range internal {
		if !isInternal(parseIP(t, ip)) {
			t.Errorf("isInternal(%s) = false, want true", ip)
		}
	}
	for _, ip := range public {
		if isInternal(parseIP(t, ip)) {
			t.Errorf("isInternal(%s) = true, want false", ip)
		}
	}
}

func parseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("could not parse %q as an IP", s)
	}
	return ip
}
