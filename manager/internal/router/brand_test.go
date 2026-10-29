// SPDX-License-Identifier: MIT

package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
)

// Trimmed OpenCode 2.2.0 index.html (the parts brandHTML relies on).
const ocIndex = `<!doctype html>
<html lang="en"><head><meta charset="utf-8" /><title data-sm>OpenCode</title>
<script type="module" crossorigin src="/_assets/index-DSXrPRcR.js"></script></head>
<body class="antialiased"><div id="root" class="flex flex-col h-dvh"></div></body></html>`

func TestBrandHTML(t *testing.T) {
	got := string(brandHTML([]byte(ocIndex), "https://apps.example.org/", "de"))
	for _, want := range []string{
		`<title data-sm>Agent Workspace &ndash; OpenCode</title>`,
		`<link rel="stylesheet" href="/__workspace/brand.css">`,
		`<script src="/__workspace/title.js" defer></script></head>`,
		`<a id="workspace-portal" href="https://apps.example.org/">Agent Workspace</a>`,
		`<a id="workspace-notice" href="https://apps.example.org/privacy#prototype">` + consent.Text("de").Short + `</a></body>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "<style") || strings.Count(got, "<script") != len(regexp.MustCompile(`<script[^>]*\ssrc=`).FindAllString(got, -1)) {
		t.Error("nothing inline may be added (CSP)")
	}
	broken := `<html><head><title>x</title></head><div id="root">`
	if got := string(brandHTML([]byte(broken), "u", "de")); got != broken {
		t.Errorf("document without </body> must stay untouched: %s", got)
	}
}

func TestBrandProxy(t *testing.T) {
	var lastAE string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAE = r.Header.Get("Accept-Encoding")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "text/html") // must still pass through untouched
			io.WriteString(w, "<html><head></head><body>api</body></html>")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		io.WriteString(w, ocIndex)
	}))
	defer backend.Close()
	rt, lc := newRouter(backend.URL, true)
	get := func(path string, html bool) *httptest.ResponseRecorder {
		r := authed("GET", "https://alice.apps.example.org"+path)
		r.Header.Set("Accept-Encoding", "gzip, br")
		if html {
			r.Header.Set("Accept", "text/html,application/xhtml+xml")
		}
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		return w
	}

	w := get("/session/abc", true)
	if !strings.Contains(w.Body.String(), `id="workspace-portal"`) || lastAE != "" {
		t.Fatalf("document not branded or still compressed (Accept-Encoding %q): %s", lastAE, w.Body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp != "default-src 'self'; frame-src 'self' blob:" {
		t.Fatalf("CSP must pass through, only frame-src added for the PDF viewer: %s", csp)
	}
	if w := get("/api/session", true); strings.Contains(w.Body.String(), "workspace-portal") {
		t.Fatal("API response rewritten")
	}
	get("/_assets/x.js", false)
	if lastAE != "gzip, br" {
		t.Fatalf("asset requests keep compression, got %q", lastAE)
	}

	// Manager-owned paths work even while the container is stopped.
	lc.running = false
	if w := get("/portal", true); w.Code != 302 || w.Header().Get("Location") != "https://apps.example.org/" {
		t.Fatalf("/portal: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := get("/__workspace/brand.css", false); w.Code != 200 || !strings.Contains(w.Body.String(), "#workspace-portal") {
		t.Fatalf("brand.css: %d", w.Code)
	}
	if w := get("/__workspace/brand.js", false); w.Code != 404 { // D50: no script sets a language
		t.Fatalf("brand.js must be gone: %d", w.Code)
	}
	if w := get(brandTitlePath(), false); w.Code != 200 || w.Body.String() != brandTitleScript() || w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("title.js: %d %s", w.Code, w.Body)
	}
	if w := get("/sw.js", false); w.Code != 200 || strings.Contains(w.Body.String(), "fetch") {
		t.Fatalf("sw.js must be the no-op worker: %d %s", w.Code, w.Body)
	}
	if w := get("/__workspace/nope", false); w.Code != 404 {
		t.Fatalf("unknown brand path: %d", w.Code)
	}
}

// Contract test against a real OpenCode: OC_BRAND_IT=http://127.0.0.1:4096
// OPENCODE_PASSWORD=... go test -run BrandContract ./internal/router/
// Run it for every new OpenCode version before rolling the runtime image.
func TestBrandContract(t *testing.T) {
	base := os.Getenv("OC_BRAND_IT")
	if base == "" {
		t.Skip("OC_BRAND_IT not set")
	}
	fetch := func(path string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Accept", "text/html")
		req.SetBasicAuth("opencode", os.Getenv("OPENCODE_PASSWORD"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, doc := fetch("/session/x")
	for _, want := range []string{"<title data-sm>", "</head>", "</body>", `<div id="root"`, "h-dvh", "dataset.colorScheme", "dataset.theme"} {
		if !strings.Contains(doc, want) {
			t.Errorf("index.html lost %q: branding degrades or #root offset breaks", want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "style-src 'self'") || !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP no longer allows the same-origin stylesheet and script: %s", csp)
	}
	if !regexp.MustCompile(`img-src [^;]*data:`).MatchString(csp) {
		t.Errorf("CSP no longer allows data: images: the mark in the portal strip (brand.css) disappears: %s", csp)
	}
	if strings.Contains(csp, "frame-src") && !strings.Contains(csp, pdfFrameSrc) {
		t.Errorf("CSP now sets its own frame-src: check that the PDF viewer (blob: iframe) still works, then drop pdfFrameSrc: %s", csp)
	}
	entry := regexp.MustCompile(`src="(/_assets/index-[^"]+\.js)"`).FindStringSubmatch(doc)
	if entry == nil {
		t.Fatal("entry chunk not found in index.html")
	}
	if _, js := fetch(entry[1]); !regexp.MustCompile("register\\([\"`']/sw\\.js").MatchString(js) {
		t.Error(`entry chunk no longer registers "/sw.js": update swPath, else the upstream worker serves a cached unbranded index.html`)
	}

	// Every token and hook brand.css relies on must still be in the
	// served assets. Lazy chunks are found by following the asset names inside
	// each chunk, so the whole app is searched, not only the start page.
	assets := assetText(t, doc, fetch)
	for _, name := range brandCSSNames(t) {
		if !strings.Contains(assets, name) {
			t.Errorf("custom property %s is gone from the OpenCode assets: brand.css no longer applies there", name)
		}
	}
	for _, v := range brandCSSHooks(t) {
		if !regexp.MustCompile(`["'\x60=]` + regexp.QuoteMeta(v) + `["'\x60 >\]]`).MatchString(assets) {
			t.Errorf("selector value %q is gone from the OpenCode assets: a Workspace rule in brand.css (theme, hidden setting) is now dead", v)
		}
	}

	// The picker shows a model unasked only as the newest of its family
	// (models.tsx latestModels groups missing families together); see modelfix.go.
	_, list := fetch(modelListPath)
	var models struct {
		Data []struct{ ID, Family string }
	}
	if err := json.Unmarshal([]byte(list), &models); err != nil || len(models.Data) == 0 {
		t.Fatalf("model list: %v %.200s", err, list)
	}
	fams := map[string]bool{}
	for _, m := range models.Data {
		if m.Family == "" || fams[m.Family] {
			t.Errorf("model %s: family %q missing or shared, the model picker hides it", m.ID, m.Family)
		}
		fams[m.Family] = true
	}
}

// assetText returns all JS and CSS reachable from index.html, concatenated.
func assetText(t *testing.T, doc string, fetch func(string) (*http.Response, string)) string {
	ref := regexp.MustCompile(`(?:/_assets/|["'\x60]\./)([A-Za-z0-9_.-]+\.(?:js|css))\b`)
	seen := map[string]bool{}
	queue := []string{doc}
	var all strings.Builder
	for len(queue) > 0 {
		text := queue[0]
		queue = queue[1:]
		for _, m := range ref.FindAllStringSubmatch(text, -1) {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			resp, body := fetch("/_assets/" + m[1])
			if resp.StatusCode != http.StatusOK {
				continue // a "./x.js" string that is not an asset
			}
			all.WriteString(body)
			queue = append(queue, body)
		}
	}
	if len(seen) < 50 {
		t.Fatalf("only %d assets found: the chunk naming changed, fix assetText", len(seen))
	}
	return all.String()
}

// brandCSSNames lists the OpenCode custom properties brand.css sets or reads.
func brandCSSNames(t *testing.T) []string {
	var out []string
	for _, m := range regexp.MustCompile(`--[a-z0-9-]+`).FindAllString(string(brandCSS), -1) {
		if !strings.HasPrefix(m, "--workspace-") && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	if len(out) < 10 {
		t.Fatalf("brand.css tokens not found: %v", out)
	}
	return out
}

// brandCSSHooks lists the attribute values brand.css selects on (data-slot,
// data-value, ...); light/dark are set by OpenCode's preload script, checked above.
func brandCSSHooks(t *testing.T) []string {
	var out []string
	for _, m := range regexp.MustCompile(`\[(data-[a-z-]+)="([^"]+)"\]`).FindAllStringSubmatch(string(brandCSS), -1) {
		if m[1] != "data-color-scheme" && !slices.Contains(out, m[2]) {
			out = append(out, m[2])
		}
	}
	if len(out) < 5 {
		t.Fatalf("brand.css hooks not found: %v", out)
	}
	return out
}

func TestInstanceBranding(t *testing.T) {
	d, err := instance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	got := string(brandHTML([]byte(ocIndex), "https://apps.example.org/", "de"))
	if strings.Contains(strings.ToLower(got), "\x7a\x6b\x6d") || !strings.Contains(got, "Agent Workspace") || !strings.Contains(got, "/__workspace/brand.css") {
		t.Fatalf("wrong instance branding: %s", got)
	}
	d.Values["name"] = `<script>alert("x")</script>`
	got = string(brandHTML([]byte(ocIndex), "https://apps.example.org/", "de"))
	if strings.Contains(got, `<script>alert`) || !strings.Contains(got, "&lt;script&gt;") {
		t.Fatal("instance title injected markup")
	}
	if strings.Contains(brandTitleScript(), "</script>") {
		t.Fatal("instance title was not JSON escaped")
	}
}

// Profile entry belongs to the portal; OpenCode branding has no profile injection.
func TestBrandDoesNotLeakAssistantPresentation(t *testing.T) {
	d, err := instance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	before := string(brandHTML([]byte(ocIndex), "https://apps.example.org/", "de"))
	d.Assistants[0].EntryQuestion = `<script>humanOnly()</script>`
	d.Assistants[0].InitPrompt = "init-only"
	if got := string(brandHTML([]byte(ocIndex), "https://apps.example.org/", "de")); got != before {
		t.Fatal("profile presentation injected into general OpenCode HTML")
	}
	// Even HTML-shaped session responses must remain byte-for-byte untouched.
	rt := &Router{Domain: "apps.example.org"}
	r := httptest.NewRequest("GET", "https://alice.apps.example.org/api/session/ses_test", nil)
	r.Header.Set("Accept", "text/html")
	resp := &http.Response{Request: r, StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(ocIndex))}
	if err := rt.brandResponse(resp); err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != ocIndex {
		t.Fatal("session API response decorated")
	}
}

func TestBrandNoticeLanguages(t *testing.T) {
	d, err := instance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Select(d)()
	for _, tc := range []struct {
		name, stored, cookie, accept, want string
	}{
		{"stored English", "en", "lang=de", "de", "en"},
		{"stored German", "de", "lang=en", "en", "de"},
		{"cookie", "", "lang=en", "de", "en"},
		{"old cookie", "", "sprache=en", "de", "en"},
		{"browser English", "", "", "en-GB,en;q=0.9,de;q=0.8", "en"},
		{"browser German", "", "", "en;q=0.5,de;q=0.9", "de"},
		{"fallback", "", "lang=invalid", "fr", "de"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := New(Router{Domain: "apps.example.org", Lang: func(_ context.Context, uid string) (string, error) {
				if uid != "person" {
					t.Errorf("wrong language lookup uid: %q", uid)
				}
				return tc.stored, nil
			}})
			r := httptest.NewRequest("GET", "https://person.apps.example.org/", nil)
			r.Header.Set("Accept", "text/html")
			r.Header.Set("Accept-Language", tc.accept)
			r.Header.Set("Cookie", tc.cookie)
			pt := proxyTarget{uid: "person", lang: rt.personLanguage(r, "person"), target: lifecycle.Target{BaseURL: "http://runtime"}}
			r = r.WithContext(context.WithValue(r.Context(), proxyKey{}, pt))
			proxy := &httputil.ProxyRequest{In: r, Out: r.Clone(r.Context())}
			rewrite(proxy)
			if proxy.Out.Header.Get("Cookie") != "" {
				t.Fatal("language cookie leaked upstream")
			}
			resp := &http.Response{Request: proxy.Out, StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(ocIndex))}
			if err := rt.brandResponse(resp); err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			want := "Check results before using them"
			other := "Resultate vor der Verwendung prüfen"
			if tc.want == "de" {
				want, other = other, want
			}
			if !strings.Contains(string(body), want) || strings.Contains(string(body), other) {
				t.Errorf("wrong notice language: %s", body)
			}
			if got := translate(tc.want, "Check results before using them"); got != want {
				t.Errorf("router translation: %q, want %q", got, want)
			}
		})
	}
}
