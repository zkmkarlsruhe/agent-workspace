// SPDX-License-Identifier: MIT

package router

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// Branding of the OpenCode web UI: the proxy rewrites the HTML document
// (title prefix, one stylesheet, a link back to the portal, the short prototype notice) and serves the
// stylesheet itself under /__workspace/. No script sets a language: the UI follows
// the browser (D50). A same-origin script keeps the prefix on dynamic titles.
// Nothing inline is added; the CSP only gains frame-src for the PDF
// viewer (brandResponse).
//
// OpenCode's service worker precaches index.html with an integrity hash and
// answers every navigation from that cache, which would bypass the rewrite and
// fail its install on the changed bytes. /sw.js is therefore replaced by a
// worker that removes the old caches and never intercepts a request.

//go:embed brand.css
var brandCSS []byte

const (
	swPath       = "/sw.js"
	maxHTMLBytes = 2 << 20
)

// Kept byte-stable: a changed worker would be reinstalled on every page load.
const noopServiceWorker = `self.addEventListener("install", () => self.skipWaiting())
self.addEventListener("activate", (e) =>
  e.waitUntil(caches.keys().then((keys) => Promise.all(keys.map((k) => caches.delete(k)))).then(() => self.clients.claim())),
)
`

// OpenCode 2.2.0 replaces the preload title on mount and session navigation.
func brandTitleScript() string {
	prefix, _ := json.Marshal(instance.Value("name") + " – ")
	return `const prefix = ` + string(prefix) + `
const brandTitle = () => {
  if (document.title && !document.title.startsWith(prefix)) document.title = prefix + document.title
}
new MutationObserver(brandTitle).observe(document.head, { childList: true, subtree: true, characterData: true })
brandTitle()
`
}

// serveBrand answers the paths the manager owns on <slug>.<domain>; it reports
// false for everything that goes to OpenCode.
func (rt *Router) serveBrand(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch {
	case r.URL.Path == "/portal" || r.URL.Path == "/portal/":
		http.Redirect(w, r, rt.portalURL("/"), http.StatusFound)
	case r.URL.Path == brandCSSPath():
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, instance.Text(string(brandCSS)))
	case r.URL.Path == brandTitlePath():
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, brandTitleScript())
	case r.URL.Path == swPath:
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, noopServiceWorker)
	case strings.HasPrefix(r.URL.Path, brandPrefix()):
		http.NotFound(w, r)
	default:
		return false
	}
	return true
}

func isClear(r *http.Request) bool {
	return r.URL.Path == brandPrefix()+"clear" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
}

// clearSite runs after "Alle meine Daten löschen": drop the OpenCode UI state
// of this host, then back to the portal.
func (rt *Router) clearSite(w http.ResponseWriter, r *http.Request) {
	if site := r.Header.Get("Sec-Fetch-Site"); (site != "same-origin" && site != "same-site" && site != "none") ||
		r.Header.Get("Sec-Fetch-Mode") != "navigate" || r.Header.Get("Sec-Fetch-Dest") != "document" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Clear-Site-Data", `"cache", "storage"`)
	http.Redirect(w, r, rt.portalURL("/deleted"), http.StatusSeeOther)
}

// wantsHTML marks requests whose response may be the app document; the proxy
// asks OpenCode for identity encoding only for these.
func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") && navigation(r)
}

func (rt *Router) brandResponse(resp *http.Response) error {
	if !wantsHTML(resp.Request) || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLBytes+1))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if len(body) <= maxHTMLBytes {
		pt, _ := resp.Request.Context().Value(proxyKey{}).(proxyTarget)
		body = brandHTML(body, rt.portalURL("/"), pt.lang)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Etag")
	// OpenCode 2.2.0's CSP has no frame-src, so default-src 'self' blocks its own PDF
	// viewer, an <iframe> on a blob: URL of the file (blob documents inherit this CSP).
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "" && !strings.Contains(csp, "frame-src") {
		resp.Header.Set("Content-Security-Policy", csp+"; "+pdfFrameSrc)
	}
	return nil
}

const pdfFrameSrc = "frame-src 'self' blob:"

// brandHTML leaves the document untouched unless both </head> and </body> are
// there, so an unexpected upstream change degrades to the plain OpenCode UI.
func brandHTML(doc []byte, portal, lang string) []byte {
	head := bytes.Index(doc, []byte("</head>"))
	body := bytes.LastIndex(doc, []byte("</body>"))
	if head < 0 || body < head {
		return doc
	}
	var b bytes.Buffer
	b.Grow(len(doc) + 256)
	pre := doc[:head]
	if t := bytes.Index(pre, []byte("<title data-sm>")); t >= 0 {
		t += len("<title data-sm>")
		b.Write(pre[:t])
		b.WriteString(html.EscapeString(instance.Value("name")) + " &ndash; ")
		pre = pre[t:]
	}
	b.Write(pre)
	b.WriteString(`<link rel="stylesheet" href="` + brandCSSPath() + `">`)
	b.WriteString(`<script src="` + brandTitlePath() + `" defer></script>`)
	b.Write(doc[head:body])
	b.WriteString(`<a id="` + instance.Value("brand_id") + `-portal" href="` + html.EscapeString(portal) + `">` + html.EscapeString(instance.Value("name")) + `</a>`)
	b.WriteString(`<a id="` + instance.Value("brand_id") + `-notice" href="` + html.EscapeString(portal) + `privacy#prototype">` + html.EscapeString(noticeText(lang)) + `</a>`)
	b.Write(doc[body:])
	return b.Bytes()
}

func brandPrefix() string    { return instance.BrandPrefix() }
func brandCSSPath() string   { return brandPrefix() + "brand.css" }
func brandTitlePath() string { return brandPrefix() + "title.js" }

func noticeText(lang string) string {
	if short := consent.Text(lang).Short; short != "" {
		return short
	}
	return translate(lang, "Check results before using them")
}
