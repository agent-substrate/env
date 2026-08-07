package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	txt := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			txt = tc.Text
		}
	}
	if res.IsError {
		t.Fatalf("%s tool returned unexpected error: %s", toolName, txt)
	}
	return txt
}

func invoke(t *testing.T, reg *tool.Registry, toolName string, input map[string]any) *mcp.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return reg.Invoke(context.Background(), toolName, raw)
}

func TestBrowserMarkdown(t *testing.T) {
	reg, srv := browserFixture(t)
	out := call(t, reg, "browser", map[string]any{"url": srv.URL + "/page"})

	for _, want := range []string{
		"status: 200 OK",
		"title: Sample Page",
		"description: A page for tests.",
		"heading",
		"bold",
		"absolute-ised link",
		"alpha",
	} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("markdown output missing %q:\n%s", want, out)
		}
	}
}

func TestBrowserRawHTML(t *testing.T) {
	reg, srv := browserFixture(t)
	out := call(t, reg, "browser", map[string]any{"url": srv.URL + "/page", "mode": "raw"})

	if !strings.Contains(out, "<title>") {
		t.Errorf("raw output missing HTML tags:\n%s", out)
	}
}

func TestBrowserJSON(t *testing.T) {
	reg, srv := browserFixture(t)
	out := call(t, reg, "browser", map[string]any{"url": srv.URL + "/json"})

	if !strings.Contains(out, `{"ok":true}`) {
		t.Errorf("json output missing payload:\n%s", out)
	}
}

func TestBrowserBinaryRejected(t *testing.T) {
	reg, srv := browserFixture(t)
	res := invoke(t, reg, "browser", map[string]any{"url": srv.URL + "/binary"})
	if !res.IsError {
		t.Fatalf("expected binary fetch to fail, got success")
	}
}
