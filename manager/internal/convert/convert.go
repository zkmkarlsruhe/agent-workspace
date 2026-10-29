// SPDX-License-Identifier: MIT

// Package convert turns Markdown results into PDFs for people who cannot use
// .md files. Markdown is rendered to HTML here (goldmark, raw HTML dropped)
// and printed by the shared Gotenberg sidecar on an internal network; Chromium
// there runs without JavaScript and may only load its own temp files.
package convert

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	gohtml "html"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
)

const (
	MaxMarkdown = 4 << 20  // larger reports are not "a report" any more
	maxPDF      = 64 << 20 // answer cap from Gotenberg
	timeout     = 60 * time.Second
)

var (
	ErrTooLarge = errors.New("markdown too large")
	ErrBackend  = errors.New("pdf service failed")
)

//go:embed print.css
var printCSS string

//go:embed house.css
var houseCSS string

type Converter struct {
	URL  string // Gotenberg base, e.g. http://gotenberg:3000
	HTTP *http.Client
	Now  func() time.Time
}

func New(url string) *Converter {
	return &Converter{URL: strings.TrimRight(url, "/"), HTTP: &http.Client{Timeout: timeout, Transport: logx.Transport{Upstream: "gotenberg", Propagate: true, Body: true}}, Now: time.Now}
}

// goldmark's default renderer drops raw HTML and refuses javascript:/data:
// link targets; never add html.WithUnsafe here.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(altText{}, 0))),
)

// altText prints images as "[Bild: alt]": only index.html reaches Gotenberg,
// so project images cannot load and remote ones are blocked there.
type altText struct{}

func (altText) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindImage, func(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			w.WriteString("[Bild: " + gohtml.EscapeString(plain(n, src)) + "]")
		}
		return ast.WalkSkipChildren, nil
	})
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="de"><head><meta charset="utf-8"><title>{{.Title}}</title><style>{{.CSS}}</style></head>
<body><main>{{.Body}}</main></body></html>`))

// Chromium renders the footer at a different zoom: 10mm padding lands on the
// 20mm page margin (checked with pdftoppm).
var footer = template.Must(template.New("footer").Parse(`<!doctype html>
<html lang="de"><head><meta charset="utf-8"><style>
html,body{margin:0;padding:0;width:100%}
div{box-sizing:border-box;width:100%;padding:0 10mm;display:flex;justify-content:space-between;font:7.5pt "Open Sans","Liberation Sans",Arial,Helvetica,sans-serif;color:#555}
</style></head><body><div><span>{{.Made}} {{.Instance}} &middot; {{.Date}}</span><span>{{.Page}} <span class="pageNumber"></span> {{.Of}} <span class="totalPages"></span></span></div></body></html>`))

var months = [...]string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"}

func germanDate(t time.Time) string {
	return fmt.Sprintf("%d. %s %d", t.Day(), months[t.Month()-1], t.Year())
}

type langKey struct{}

// WithLang sets the footer language of conversions under ctx ("en", else
// German); the portal passes the person's choice, tools keep German.
func WithLang(ctx context.Context, lang string) context.Context {
	return context.WithValue(ctx, langKey{}, lang)
}

func footerText(ctx context.Context, t time.Time) map[string]string {
	if l, _ := ctx.Value(langKey{}).(string); l == "en" {
		return map[string]string{"Instance": instance.Value("name"), "Made": "Created with", "Date": t.Format("2 January 2006"), "Page": "Page", "Of": "of"}
	}
	return map[string]string{"Instance": instance.Value("name"), "Made": "Erstellt mit", "Date": germanDate(t), "Page": "Seite", "Of": "von"}
}

// HTML renders a Markdown document into the print page; fallback is the title
// when the document has no heading (usually the file name).
func HTML(src []byte, fallback string) (string, error) {
	doc := md.Parser().Parse(text.NewReader(src))
	title := fallback
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if h, ok := n.(*ast.Heading); ok && entering {
			title = plain(h, src)
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	var body bytes.Buffer
	if err := md.Renderer().Render(&body, src, doc); err != nil {
		return "", err
	}
	var out bytes.Buffer
	css := printCSS
	if instance.Current().Enabled("workspace-style") {
		css = houseCSS
	}
	err := page.Execute(&out, map[string]any{"Title": title, "CSS": template.CSS(css), "Body": template.HTML(body.String())})
	return out.String(), err
}

// Fragment renders safe Markdown using the same HTML/link/image policy as exports.
func Fragment(src []byte) (template.HTML, error) {
	if len(src) > MaxMarkdown {
		return "", ErrTooLarge
	}
	var out bytes.Buffer
	doc := md.Parser().Parse(text.NewReader(src))
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if h, ok := n.(*ast.Heading); ok && entering {
			h.Level = min(h.Level+1, 6)
			if id, ok := h.AttributeString("id"); ok {
				if value, ok := id.([]byte); ok {
					h.SetAttributeString("id", append([]byte("knowledge-heading-"), value...))
				}
			}
		}
		return ast.WalkContinue, nil
	})
	if err := md.Renderer().Render(&out, src, doc); err != nil {
		return "", err
	}
	return template.HTML(out.String()), nil
}

func plain(n ast.Node, src []byte) string {
	var b strings.Builder
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if t, ok := c.(*ast.Text); ok && entering {
			b.Write(t.Segment.Value(src))
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

// MarkdownToPDF converts one Markdown document. name is used as title when
// the document has no heading.
func (c *Converter) MarkdownToPDF(ctx context.Context, src []byte, name string) ([]byte, error) {
	if len(src) > MaxMarkdown {
		return nil, ErrTooLarge
	}
	html, err := HTML(src, name)
	if err != nil {
		return nil, err
	}
	var foot bytes.Buffer
	footer.Execute(&foot, footerText(ctx, c.Now().In(instance.Location())))

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range []struct{ name, data string }{{"index.html", html}, {"footer.html", foot.String()}} {
		w, _ := mw.CreateFormFile("files", f.name)
		io.WriteString(w, f.data)
	}
	for _, kv := range [][2]string{
		{"paperWidth", "210mm"}, {"paperHeight", "297mm"},
		{"marginTop", "20mm"}, {"marginBottom", "22mm"}, {"marginLeft", "20mm"}, {"marginRight", "20mm"},
		{"printBackground", "true"}, {"emulatedMediaType", "print"}, {"generateDocumentOutline", "true"},
		{"failOnConsoleExceptions", "false"},
	} {
		mw.WriteField(kv[0], kv[1])
	}
	mw.Close()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/forms/chromium/convert/html", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackend, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrBackend, resp.StatusCode, bytes.TrimSpace(msg))
	}
	pdf, err := io.ReadAll(io.LimitReader(resp.Body, maxPDF+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackend, err)
	}
	if len(pdf) > maxPDF || !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		return nil, fmt.Errorf("%w: no or oversized PDF (%d bytes)", ErrBackend, len(pdf))
	}
	return pdf, nil
}
