package browser

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
)

var markdownConverter = sync.OnceValue(func() *converter.Converter {
	return converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(),
		strikethrough.NewStrikethroughPlugin(),
	))
})

func toMarkdown(ctx context.Context, doc string, base *url.URL) (string, error) {
	opts := []converter.ConvertOptionFunc{converter.WithContext(ctx)}
	if base != nil {
		domain := base.Scheme + "://" + base.Host
		opts = append(opts, converter.WithDomain(domain))
	}
	md, err := markdownConverter().ConvertString(doc, opts...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(md), nil
}

var (
	titleRe   = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title\s*>`)
	descRe    = regexp.MustCompile(`(?is)<meta\b[^>]*name\s*=\s*["']description["'][^>]*>`)
	contentRe = regexp.MustCompile(`(?is)content\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	tagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe   = regexp.MustCompile(`[\s\x{00a0}]+`)
)

func htmlTitle(doc string) string {
	m := titleRe.FindStringSubmatch(doc)
	if m == nil {
		return ""
	}
	return clean(tagRe.ReplaceAllString(m[1], ""))
}

func htmlDescription(doc string) string {
	tag := descRe.FindString(doc)
	if tag == "" {
		return ""
	}
	m := contentRe.FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	return clean(firstNonEmpty(m[1:]...))
}

func clean(s string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(html.UnescapeString(s), " "))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
