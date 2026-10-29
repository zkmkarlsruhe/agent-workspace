// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
)

// germanWord are words only German text has (no English homographs).
var germanWord = map[string]bool{}

func init() {
	for w := range strings.FieldsSeq(`und oder der die das den dem des ein eine einen einem einer ist sind wird werden
		nicht kein keine keinen mit für auf bei von vom zum zur ins bitte du dein deine deinen dir dich noch schon auch
		nur wenn dann dass hier jetzt alle gibt sich wie datei dateien ordner seite fehler leer`) {
		germanWord[w] = true
	}
}

// enAllow are German words that belong in the English text: tool names,
// argument names and values, project folder names.
var enAllow = []string{"web_search", "web_read", "inbox", "results",
	"mode", "cache_only", "section", "file", "folder", "path", "project", "term", "language", "time_range",
	"nextcloud_pull", "Agents", "Workspace", "Nextcloud", "Führungen", "Räume"}

func germanLeftovers(text string, allow ...string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && r != '_' }) {
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

// translated maps the functions that translate a text argument to the index
// of that argument.
var translated = map[string]int{"msg": 1, "errorResult": 1, "textResult": 1, "fail": 1, "refuse": 0, "blocked": 1, "upstreamError": 4}

// TestCatalogGo: the Go source is English (no German-looking literal outside
// i18n_de.go), every literal handed to a translating function has a German
// entry, and German entries keep the %-verbs of their English key.
func TestCatalogGo(t *testing.T) {
	// cached page content is shared between persons and stays as it is
	keep := map[string]bool{" [Bild: ": true, "] ": true, "(ohne Text)": true, " (Helfer": true}
	// German proper names and lane names the English text carries on purpose
	allow := append(enAllow, "Ausstellungen", "Schulferien", "Feiertage", "Wetter", "von", "dir", "ä", "ö", "ü", "ß", "ae", "oe", "ue", "ss")
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || strings.HasPrefix(f, "i18n_de") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				name := ""
				switch fn := c.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
					if x, _ := fn.X.(*ast.Ident); x != nil && x.Name == "regexp" {
						return false // regexes match German and English input
					}
				}
				if i, ok := translated[name]; ok && i < len(c.Args) {
					if lit, ok := c.Args[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, _ := strconv.Unquote(lit.Value); s != "" && de[s] == "" {
							t.Errorf("%s: no German for %q", fset.Position(lit.Pos()), s)
						}
					}
				}
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, _ := strconv.Unquote(lit.Value)
			if !keep[s] {
				if g := germanLeftovers(s, allow...); len(g) > 0 {
					t.Errorf("%s: German in source %q: %v", fset.Position(lit.Pos()), s, g)
				}
			}
			return true
		})
	}
	for k, v := range de {
		if strings.Count(k, "%") != strings.Count(v, "%") {
			t.Errorf("%%-verbs differ: %q -> %q", k, v)
		}
		if g := germanLeftovers(k, enAllow...); len(g) > 0 {
			t.Errorf("German in English key %q: %v", k, g)
		}
	}
}

// TestToolsCatalog: every title and description of every tool has German.
func TestToolsCatalog(t *testing.T) {
	var walk func(m map[string]any)
	walk = func(m map[string]any) {
		for k, v := range m {
			switch x := v.(type) {
			case string:
				if (k == "title" || k == "description") && de[x] == "" {
					t.Errorf("no German for %s %q", k, x)
				}
			case map[string]any:
				walk(x)
			}
		}
	}
	for _, tl := range slices.Concat(toolList, []map[string]any{mdToPDFTool, imageViewTool, imageGenDef, imageEditTool(), handwritingTool}, fileTools, nextcloudTools, nextcloudSyncTools, roomsTools, knowledgeTools) {
		walk(tl)
	}
}

func TestToolsListBothLanguages(t *testing.T) {
	lang := "de"
	s := New(Server{SearXNG: mustURL("http://127.0.0.1:1"), SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf,
		Users: users{}, Usage: &rec{}, Log: slog.New(slog.DiscardHandler), Files: &fakeFiles{},
		PDF:       func(context.Context, []byte, string) ([]byte, error) { return []byte("%PDF"), nil },
		Nextcloud: &Nextcloud{Base: mustURL("https://cloud.example"), Sync: &fakeSync{}},
		Grants: func(_ context.Context, _, l string) (map[string]bool, map[string]string) {
			web := map[string]bool{"web_search": true, "web_read": true}
			if l == "en" {
				return web, map[string]string{"md_to_pdf": "Create a PDF"}
			}
			return web, map[string]string{"md_to_pdf": "Ein PDF erstellen"}
		},
		Lang: func(context.Context, string) (string, error) { return lang, nil }})
	const me = "10.90.1.10:40000"
	call := func(body string) string { return rpc(s, me, body).Body.String() }
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	hidden := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"md_to_pdf","arguments":{"file":"a.md"}}}`
	bad := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":" "}}}`
	initReq := `{"jsonrpc":"2.0","id":4,"method":"initialize","params":{}}`

	de := call(list)
	for _, want := range []string{`"Websuche"`, `"Datei aus Nextcloud holen"`, `"path"`} {
		if !strings.Contains(de, want) {
			t.Errorf("de list: no %s", want)
		}
	}
	if b := call(hidden); !strings.Contains(b, "ist ausgeschaltet") {
		t.Errorf("de hidden: %s", b)
	}

	lang = "en"
	var res struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(call(list)), &res); err != nil || len(res.Result.Tools) != 7 {
		t.Fatalf("en list: %v %d", err, len(res.Result.Tools))
	}
	b, _ := json.Marshal(res.Result.Tools)
	for _, want := range []string{`"Web search"`, `"Fetch file from Nextcloud"`, `"name":"nextcloud_fetch"`, `"path"`, `"term"`, `"news"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("en list: no %s", want)
		}
	}
	for _, tl := range res.Result.Tools {
		d, _ := json.Marshal(map[string]any{"t": tl["title"], "d": tl["description"], "s": tl["inputSchema"]})
		if g := germanLeftovers(strings.NewReplacer(`<`, "<", `>`, ">").Replace(string(d)), append(enAllow, "general", "news", "science", "day", "week", "month", "year", "query", "count", "domains", "page", "url", "links")...); len(g) > 0 {
			t.Errorf("en %s: German left: %v", tl["name"], g)
		}
	}
	if !strings.Contains(toolList[0]["title"].(string), "Web search") {
		t.Error("localize changed the shared English list")
	}
	if b := call(hidden); !strings.Contains(b, `The tool \"Create a PDF\" is switched off. You can switch it on in the portal under Capabilities.`) {
		t.Errorf("en hidden: %s", b)
	}
	if b := call(bad); !strings.Contains(b, "The query must be 1 to 400 characters long.") {
		t.Errorf("en error: %s", b)
	}
	if b := call(initReq); !strings.Contains(b, "Example tools for web research.") {
		t.Errorf("en instructions: %s", b)
	}
}

func TestWebReadEnglish(t *testing.T) {
	s, _ := testWeb(t)
	ctx := withLang(context.Background(), "en")
	res, _ := s.webRead(ctx, user, json.RawMessage(`{"url":"http://public.test:8080/"}`))
	text := res.Content[0].Text
	for _, want := range []string{"The following text comes from the web.", "<<<WEB_CONTENT id=", "Title: Lynn Hershman Leeson", "Section 1 of 1", "Fetched: "} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	// the cached page answers the next German call in German
	res, _ = s.webRead(context.Background(), user, json.RawMessage(`{"url":"http://public.test:8080/"}`))
	if !strings.Contains(res.Content[0].Text, "Titel: Lynn Hershman Leeson") {
		t.Errorf("German after English: %s", res.Content[0].Text)
	}
	res, _ = s.webRead(ctx, user, json.RawMessage(`{"url":"http://10.0.0.1/"}`))
	if !res.IsError || res.Content[0].Text != "IP addresses are not allowed, only host names." {
		t.Errorf("blocked: %+v", res)
	}
}

func TestHumanBytes(t *testing.T) {
	en := withLang(context.Background(), "en")
	for n, want := range map[int64]string{3 << 19: "1,5 MB", 10: "10 Byte"} {
		if got := humanBytes(context.Background(), n); got != want {
			t.Errorf("de %d: %q", n, got)
		}
	}
	for n, want := range map[int64]string{3 << 19: "1.5 MB", 10: "10 bytes", 2048: "2 KB"} {
		if got := humanBytes(en, n); got != want {
			t.Errorf("en %d: %q", n, got)
		}
	}
}
