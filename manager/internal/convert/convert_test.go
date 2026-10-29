// SPDX-License-Identifier: MIT

package convert

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const sample = `# Bericht Besucherzahlen

Text mit [Link](https://example.org/ausstellungen) und <script>alert(1)</script> und <b onclick="x()">fett</b>.

<iframe src="https://evil.example/"></iframe>

| Monat | Besuche |
|-------|--------:|
| Januar | 1.200 |

[gefährlich](javascript:alert(1))

![Pixel & Co](https://images.example/p.png)
`

func TestHTMLEscapesRawHTML(t *testing.T) {
	h, err := HTML([]byte(sample), "bericht.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script", "<iframe", "onclick", "javascript:", "<b ", "<img", "images.example"} {
		if strings.Contains(h, bad) {
			t.Errorf("html contains %q", bad)
		}
	}
	for _, want := range []string{"<title>Bericht Besucherzahlen</title>", `<a href="https://example.org/ausstellungen">Link</a>`, "<table>", `<td style="text-align:right">1.200</td>`, `lang="de"`, "@page", "[Bild: Pixel &amp; Co]"} {
		if !strings.Contains(h, want) {
			t.Errorf("html lacks %q", want)
		}
	}
	if h, _ := HTML([]byte("nur Text"), "notiz.md"); !strings.Contains(h, "<title>notiz.md</title>") {
		t.Error("fallback title missing")
	}
}

func TestMarkdownToPDF(t *testing.T) {
	var got struct {
		path, index, footer string
		fields              map[string]string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		got.fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			got.fields[k] = v[0]
		}
		for _, fh := range r.MultipartForm.File["files"] {
			f, _ := fh.Open()
			b, _ := io.ReadAll(f)
			switch fh.Filename {
			case "index.html":
				got.index = string(b)
			case "footer.html":
				got.footer = string(b)
			}
		}
		w.Write([]byte("%PDF-1.7 fake"))
	}))
	defer srv.Close()
	c := New(srv.URL + "/")
	c.Now = func() time.Time { return time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC) }
	pdf, err := c.MarkdownToPDF(context.Background(), []byte(sample), "bericht.md")
	if err != nil || string(pdf) != "%PDF-1.7 fake" {
		t.Fatalf("pdf %q err %v", pdf, err)
	}
	if got.path != "/forms/chromium/convert/html" {
		t.Errorf("path %q", got.path)
	}
	if !strings.Contains(got.index, "<table>") || strings.Contains(got.index, "<script") {
		t.Errorf("index.html wrong: %s", got.index)
	}
	if !strings.Contains(got.footer, "Erstellt mit Agent Workspace &middot; 5. März 2026") || !strings.Contains(got.footer, `class="pageNumber"`) {
		t.Errorf("footer wrong: %s", got.footer)
	}
	if got.fields["paperWidth"] != "210mm" || got.fields["paperHeight"] != "297mm" || got.fields["marginBottom"] == "" {
		t.Errorf("fields %v", got.fields)
	}
	if _, err := c.MarkdownToPDF(WithLang(context.Background(), "en"), []byte(sample), "bericht.md"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.footer, "Created with Agent Workspace &middot; 5 March 2026") || !strings.Contains(got.footer, "Page <span class=\"pageNumber\"></span> of") {
		t.Errorf("English footer wrong: %s", got.footer)
	}
}

// OC_PDF_IT=http://127.0.0.1:3999 go test -run Live: real Gotenberg; OC_PDF_OUT keeps the PDF.
func TestLive(t *testing.T) {
	u := os.Getenv("OC_PDF_IT")
	if u == "" {
		t.Skip("OC_PDF_IT not set")
	}
	src, err := os.ReadFile("testdata/bericht.md")
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := New(u).MarkdownToPDF(context.Background(), src, "bericht.md")
	if err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("OC_PDF_OUT"); out != "" {
		if err := os.WriteFile(out, pdf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d bytes", len(pdf))
}

func TestMarkdownToPDFErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("x") == "" {
			http.Error(w, "boom", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	if _, err := c.MarkdownToPDF(context.Background(), []byte("# a"), "a.md"); !errors.Is(err, ErrBackend) {
		t.Errorf("503: %v", err)
	}
	if _, err := c.MarkdownToPDF(context.Background(), make([]byte, MaxMarkdown+1), "a.md"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("large: %v", err)
	}
}
