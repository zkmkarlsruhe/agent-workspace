// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/evaluation"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// germanWord are words that only German text has (no English homographs).
var germanWord = map[string]bool{}

func init() {
	for w := range strings.FieldsSeq(`und oder der die das den dem des ein eine einen einem einer ist sind wird werden
		nicht kein keine keinen mit für auf bei von vom zum zur ins bitte du dein deine deinen deiner dir dich wir uns
		noch schon auch nur wenn dann dass hier jetzt alle über gibt kannst musst sich wie wird wurde wurden zurück
		datei dateien projekt projekte umgebung helfer papierkorb verbrauch ordner results inbox anlegen löschen
		hochladen herunterladen abbrechen speichern verbunden fähigkeit fähigkeiten einrichtung tage stunden minuten
		heute gestern uhr seite fehler sie ihnen`) {
		germanWord[w] = true
	}
}

// germanLeftovers lists the words of text that look German. allow holds
// words that are right in English too (names, folder names, du/Sie).
func germanLeftovers(text string, allow ...string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) }) {
		lw := strings.ToLower(w)
		if slices.Contains(allow, w) || slices.Contains(allow, lw) {
			continue
		}
		if germanWord[lw] || strings.ContainsAny(w, "äöüßÄÖÜ") {
			out = append(out, w)
		}
	}
	return out
}

// enAllow are German words that belong in the English text.
var enAllow = []string{"du", "Sie", "Example", "results", "inbox", "Agents", "Workspace", "Operations", "Nextcloud", "Umweltbundesamt", "TREMOD", "Göttingen", "Helfer", "generiert"}

// markup is skipped by the leftover check: URLs in links stay German.
var markup = regexp.MustCompile(`<[^>]*>`)

var tCall = regexp.MustCompile(`(?:\{\{-?|\()\s*(?:t|th)\s+("(?:[^"\\]|\\.)*")`)

func templateKeys(t *testing.T) map[string][]string {
	t.Helper()
	files, _ := filepath.Glob("../../web/templates/*.html")
	keys := map[string][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tCall.FindAllStringSubmatch(string(b), -1) {
			k, err := strconv.Unquote(m[1])
			if err != nil {
				t.Fatalf("%s: %s: %v", f, m[1], err)
			}
			keys[k] = append(keys[k], filepath.Base(f))
		}
	}
	return keys
}

func verbs(s string) int { return strings.Count(s, "%") - 2*strings.Count(s, "%%") }

func TestCatalogTemplates(t *testing.T) {
	keys := templateKeys(t)
	if len(keys) < 50 {
		t.Fatalf("only %d template keys found", len(keys))
	}
	for k, files := range keys {
		if catalogDE[k] == "" {
			t.Errorf("%v: no German for %q", files, k)
		}
	}
}

// TestCatalogGo: every text literal of the portal's Go code (notices, field
// errors, error pages) is English with a German entry in the catalog; render
// translates them. Fragments glued with + fail here, so texts with values use
// trf with a format string. The profile form options still arrive German and
// need their English.
func TestCatalogGo(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	cats := map[string]bool{}
	for k, v := range categoryEN {
		cats[k], cats[v] = true, true
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || strings.HasPrefix(f, "i18n_de") {
			continue
		}
		skip := map[*ast.BasicLit]bool{}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && (x.Name == "slog" || x.Name == "errors" || x.Name == "fmt" && sel.Sel.Name == "Errorf") {
						return false // logs and Go errors are English, never shown
					}
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "strings" {
						for _, a := range c.Args {
							if l, ok := a.(*ast.BasicLit); ok {
								skip[l] = true // markers matched in data, not shown
							}
						}
					}
				}
			}
			if f, ok := n.(*ast.Field); ok && f.Tag != nil {
				f.Tag = nil
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !strings.HasPrefix(lit.Value, `"`) {
				return true
			}
			s, _ := strconv.Unquote(lit.Value)
			if skip[lit] || notText(s) {
				return true
			}
			if catalogDE[s] == "" && (len(germanLeftovers(s)) > 0 || strings.Contains(strings.TrimSpace(s), " ")) && !goText[s] && !cats[s] {
				t.Errorf("%s: no German for %q", fset.Position(lit.Pos()), s)
			}
			return true
		})
	}
	var labels []string
	for _, o := range settings.AddressForms {
		labels = append(labels, o.Label)
	}
	for _, o := range settings.Languages {
		labels = append(labels, o.Label)
	}
	for _, o := range settings.Tones {
		labels = append(labels, o.Label)
	}
	for _, o := range settings.Lengths {
		labels = append(labels, o.Label)
	}
	for _, o := range settings.Formats {
		labels = append(labels, o.Label)
	}
	for _, o := range settings.Tasks {
		labels = append(labels, o.Label)
	}
	for _, l := range labels {
		if g := germanLeftovers(tr(langEN, l), "du", "Sie"); len(g) > 0 {
			t.Errorf("profile option %q has no English", l)
		}
	}
}

// notText is a literal that is no visible text: routes, paths, template and
// field names, query values, single letters.
// goText are literals with spaces that are no visible text.
var goText = map[string]bool{"text/markdown; charset=utf-8": true, "text/html; charset=utf-8": true, "2 Jan 2006": true, ", 15:04": true,
	"; base-uri 'none'": true, " bis ": true, // carRange spells both languages out
	"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; form-action ": true}

func notText(s string) bool {
	if strings.HasPrefix(s, "GET ") || strings.HasPrefix(s, "POST ") {
		return true
	}
	return !strings.Contains(s, " ") && (strings.ToLower(s) == s || strings.ContainsAny(s, "/?=.") || utf8.RuneCountInString(s) == 1)
}

func TestCatalogClean(t *testing.T) {
	if len(deConflicts) > 0 {
		t.Errorf("translated differently in two areas: %q", deConflicts)
	}
	for k, v := range catalogDE {
		switch {
		case strings.TrimSpace(v) == "" && strings.TrimSpace(k) != "":
			t.Errorf("empty German for %q", k)
		case verbs(k) != verbs(v):
			t.Errorf("%%-verbs differ: %q -> %q", k, v)
		}
		if g := germanLeftovers(html.UnescapeString(markup.ReplaceAllString(k, " ")), enAllow...); len(g) > 0 {
			t.Errorf("German in English text %q: %v", k, g)
		}
		// catalogEN reads German data back; an English text that is also
		// another entry's German would turn into that entry
		if e := catalogEN[k]; e != "" && e != k {
			t.Errorf("English %q is the German of %q", k, e)
		}
	}
	if len(catalogEN) != len(catalogDE) {
		t.Errorf("%d English texts share a German one", len(catalogDE)-len(catalogEN))
	}
}

func TestAcceptLang(t *testing.T) {
	for h, want := range map[string]string{
		"":                          "de",
		"de-DE,de;q=0.9,en;q=0.8":   "de",
		"en-US,en;q=0.9":            "en",
		"en-GB,en;q=0.9,de;q=0.8":   "en",
		"fr-FR,fr;q=0.9,en;q=0.5":   "en",
		"fr-FR":                     "de",
		"de;q=0.5,en;q=0.5":         "de",
		"en;q=0,de;q=0.1":           "de",
		"EN":                        "en",
		"de-CH, en;q=0.9, *;q=0.1":  "de",
		"en;q=0.8, de;q=0.9, en-gb": "en",
	} {
		if got := acceptLang(h); got != want {
			t.Errorf("acceptLang(%q) = %q, want %q", h, got, want)
		}
	}
}

func TestConsentVersionAcrossLanguages(t *testing.T) {
	// the version itself is pinned in internal/consent; here: the English page accepts the same version
	de, en := consent.Text("de"), consent.Text("en")
	if len(en.Points) != len(de.Points) || en.Title == de.Title || en.Short == "" {
		t.Fatalf("English notice must translate the German one point by point: %+v", en)
	}
	h, _ := fullSetup(t)
	w := do(t, h, req{path: "/consent", header: map[string]string{"Accept-Language": "en"}})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, html.EscapeString(en.Title), html.EscapeString(en.Points[1]), `lang="en"`)
}

type fakePrefs struct {
	lang     map[string]string
	account  bool
	setCalls int
	langErr  error
}

func (f *fakePrefs) Lang(_ context.Context, uid string) (string, error) {
	return f.lang[uid], f.langErr
}
func (f *fakePrefs) UserByUID(_ context.Context, uid string) (store.User, error) {
	if !f.account {
		return store.User{}, store.ErrNotFound
	}
	return store.User{UID: uid}, nil
}
func (f *fakePrefs) SetLang(_ context.Context, uid, l string) error {
	f.setCalls++
	if f.account {
		f.lang[uid] = l
	}
	return nil
}

func TestLangSwitch(t *testing.T) {
	fp := &fakePrefs{lang: map[string]string{}}
	h, err := New(Config{Backend: newFake(), Prefs: fp, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if b := do(t, h, req{path: "/"}).Body.String(); !strings.Contains(b, `lang="de"`) || !strings.Contains(b, `action="/lang"`) {
		t.Fatal("German by default, with the switch")
	}
	wantBody(t, do(t, h, req{path: "/", header: map[string]string{"Accept-Language": "en-GB,en"}}), `lang="en"`, "My projects")

	// before an account exists: cookie only
	w := do(t, h, req{method: "POST", path: "/lang", form: url.Values{"lang": {"en"}, "next": {"/consent?next=/x"}}, header: sameOrigin})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/consent?next=/x" {
		t.Errorf("back to %q", loc)
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != langCookie || c[0].Value != "en" || !c[0].HttpOnly || !c[0].Secure {
		t.Fatalf("cookie %+v", c)
	}
	if len(fp.lang) != 0 {
		t.Error("stored without an account")
	}
	cookie := map[string]string{"Cookie": "sprache=en", "Accept-Language": "de"}
	wantBody(t, do(t, h, req{path: "/", header: cookie}), `lang="en"`)

	// with an account: the first request copies the cookie, then the stored choice wins
	fp.account = true
	do(t, h, req{path: "/", header: cookie})
	if fp.lang[anna.UID] != "en" {
		t.Fatalf("cookie not copied: %v", fp.lang)
	}
	w = do(t, h, req{method: "POST", path: "/lang", form: url.Values{"lang": {"de"}, "next": {"//evil.example/"}}, header: sameOrigin})
	if w.Header().Get("Location") != "/" || fp.lang[anna.UID] != "de" {
		t.Errorf("switch to de: %q %v", w.Header().Get("Location"), fp.lang)
	}
	wantBody(t, do(t, h, req{path: "/", header: cookie}), `lang="de"`)

	wantStatus(t, do(t, h, req{method: "POST", path: "/lang", form: url.Values{"lang": {"fr"}}, header: sameOrigin}), http.StatusBadRequest)
	w = do(t, h, req{method: "POST", path: "/lang", form: url.Values{"lang": {"en"}}, header: map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}})
	wantStatus(t, w, http.StatusForbidden)
	if fp.lang[anna.UID] != "de" {
		t.Error("cross-site switch changed the language")
	}
	// works without an identity (no-access page)
	wantStatus(t, do(t, h, req{method: "POST", path: "/lang", form: url.Values{"lang": {"en"}}, header: sameOrigin, id: &Identity{}}), http.StatusSeeOther)
}

type fakeAdmin struct{}

func (fakeAdmin) Overview(context.Context, int) ([]evaluation.Day, error) {
	return []evaluation.Day{{Day: time.Now()}}, nil
}
func (fakeAdmin) Chats(context.Context, evaluation.Filter) (evaluation.ChatList, error) {
	return evaluation.ChatList{Items: []evaluation.Item{{ChatRow: store.ChatRow{Username: "anna", Chat: store.Chat{SessionID: "ses_1", Title: "report", Updated: time.Now()}}, Flags: []string{"aborted"}}}}, nil
}
func (fakeAdmin) Chat(context.Context, evaluation.Reader, string) (evaluation.Item, error) {
	return evaluation.Item{ChatRow: store.ChatRow{Username: "anna", Chat: store.Chat{SessionID: "ses_1", Title: "report", Transcript: "## anna\nhello"}}}, nil
}
func (fakeAdmin) Report(context.Context, time.Time) (string, error) { return "# report", nil }
func (fakeAdmin) Start() error                                      { return nil }
func (fakeAdmin) State() (bool, evaluation.RunResult) {
	return false, evaluation.RunResult{At: time.Now(), LLM: 2}
}
func (fakeAdmin) Audit(context.Context, int) ([]store.AuditEntry, error) {
	return []store.AuditEntry{{At: time.Now(), AdminName: "anna", SessionID: "ses_1", Do: "read"}}, nil
}

type fakeConsent struct{ at time.Time }

func (f *fakeConsent) Accepted(context.Context, string) (time.Time, bool, error) {
	return f.at, !f.at.IsZero(), nil
}
func (f *fakeConsent) Accept(context.Context, string) error { f.at = time.Now(); return nil }

// fullSetup has every optional part, with neutral test data (no German words).
func fullSetup(t *testing.T) (http.Handler, *fake) {
	t.Helper()
	f := newFake()
	f.addProject("report", "Count visitors 2025")
	f.projects["report"].LastActivity = time.Now()
	f.projects["report"].Recent = []Chat{{ID: "ses_1", Title: "visitors", Updated: time.Now(), Link: "https://anna.apps.example.org/session/ses_1"}}
	f.addProject("demo", "")
	f.trash = []TrashItem{{ID: "old--1", Name: "old", DeletedAt: time.Now(), PurgeAt: time.Now().Add(72 * time.Hour), SizeBytes: 2048, Chats: 1}}
	f.tree = []File{{Name: "a.md", Path: "results/a.md", Size: 1500, Modified: time.Now()}, {Name: "b.xlsx", Path: "inbox/b.xlsx", Size: 2 << 20, Modified: time.Now()}}
	f.usage = []UsageRow{{Project: "report", CostEUR: 1.5, CO2MinG: 6, CO2MaxG: 20, Chats: 2}, {Project: "old", Trashed: true, CostEUR: 0.5}, {CostEUR: 0.02}}
	fp := &fakeSettings{d: settings.Record{Facts: settings.Facts{Name: "Anna", Email: "anna@example.org", AsOf: time.Now()}}}
	fa := &fakeSync{connected: true, st: SyncStatus{Linked: true, Folder: "Agent Workspace/report", URL: "https://cloud.example/x", Last: time.Now()}}
	ft := &fakeTools{d: ToolsPage{Running: true,
		Standard: []Tool{{ID: "xlsx", Title: "Excel", Description: "Excel."}},
		Catalog:  []Tool{{ID: "pdf", Title: "PDF", Description: "PDF.", Category: "PDF"}},
		Own:      []OwnSkill{{ID: "mine", Description: "Mine.", Changed: time.Now()}}}}
	fc := &fakeConns{v: Connection{ID: "nextcloud", Title: "Nextcloud", Connected: true, Login: "anna", Since: time.Now()}}
	h, err := New(Config{Backend: f, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup,
		PDF:   func(context.Context, []byte, string) ([]byte, error) { return []byte("%PDF-"), nil },
		Tools: ft, Connections: fc, Onboarding: &fakeOnboarding{seen: map[string]bool{anna.UID: true}}, Sync: fa, Settings: fp,
		Consent: &fakeConsent{at: time.Now()}, DayCapEUR: 5, IdleTimeout: 20 * time.Minute, MaxRunning: 25, Admin: fakeAdmin{}, AdminGroup: "authentik Admins", Prefs: &fakePrefs{lang: map[string]string{}}})
	if err != nil {
		t.Fatal(err)
	}
	return h, f
}

// pagePaths are GET pages of the portal with the status fullSetup gives them.
var pagePaths = map[string]int{
	"/": 200, "/?template=protokoll": 200, "/projects/report": 200, "/projects/report/delete": 200, "/trash": 200,
	"/trash/old--1/delete": 200, "/environment": 200, "/environment/starting": 200, "/environment/reset": 200,
	"/environment/stop": 200, "/environment/delete": 200, "/deleted": 200, "/usage": 200, "/usage/method": 200,
	"/help": 200, "/how-it-works": 200, "/privacy": 200, "/consent": 200, "/consent/declined": 200, "/tools": 200,
	"/tools/own/mine/delete": 200, "/connections": 200, "/onboarding": 200,
	"/onboarding?step=2": 200, "/onboarding?step=3": 200, "/onboarding?step=4": 200, "/settings": 200,
	"/admin": 200, "/admin/chats": 200, "/admin/chats/ses_1": 200, "/gibt-es-nicht": 404, "/projects/fehlt": 404,
}

var (
	tagRe    = regexp.MustCompile(`(?s)<(script|style|pre|textarea|code)\b.*?</(script|style|pre|textarea|code)>|<[^>]*>`)
	attrRe   = regexp.MustCompile(`\b(?:placeholder|aria-label|title|alt)="([^"]*)"`)
	userData = strings.NewReplacer("Count visitors 2025", "", "Anna", "")
)

// visibleText is what a reader sees or hears: text plus labelling attributes,
// without code, the prefilled task text and the person's own data.
func visibleText(body string) string {
	var b strings.Builder
	for _, m := range attrRe.FindAllStringSubmatch(body, -1) {
		b.WriteString(m[1] + " ")
	}
	b.WriteString(tagRe.ReplaceAllString(body, " "))
	return userData.Replace(html.UnescapeString(b.String()))
}

func TestPagesBothLanguages(t *testing.T) {
	h, _ := fullSetup(t)
	for p, code := range pagePaths {
		for _, l := range []string{"de", "en"} {
			w := do(t, h, req{path: p, header: map[string]string{"Cookie": "sprache=" + l}})
			if w.Code != code {
				t.Errorf("%s %s: status %d, want %d", l, p, w.Code, code)
				continue
			}
			b := w.Body.String()
			if !strings.Contains(b, `lang="`+l+`"`) || !strings.Contains(b, `action="/lang"`) {
				t.Errorf("%s %s: no lang attribute or switch", l, p)
			}
			if l == "en" {
				// du/Sie are the German forms of address offered on the profile form
				if g := germanLeftovers(visibleText(b), append(enAllow, "du", "Sie")...); len(g) > 0 {
					t.Errorf("en %s: German left: %v", p, g)
				}
			}
		}
	}
}

func TestLangCookieWithoutAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"empty stored language", nil},
		{"missing account", store.ErrNotFound},
		{"lookup failed", errors.New("lookup failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakePrefs{lang: map[string]string{}, langErr: tc.err}
			h, err := New(Config{Backend: newFake(), Prefs: fp, Assets: os.DirFS("../../web")})
			if err != nil {
				t.Fatal(err)
			}
			w := do(t, h, req{path: "/", header: map[string]string{"Cookie": "lang=en"}})
			wantStatus(t, w, http.StatusOK)
			wantBody(t, w, `lang="en"`)
			if fp.setCalls != 0 {
				t.Fatalf("SetLang called %d times without an account", fp.setCalls)
			}
		})
	}
}
