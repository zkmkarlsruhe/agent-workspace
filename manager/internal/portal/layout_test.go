// SPDX-License-Identifier: MIT

package portal

import (
	"bytes"
	"cmp"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistantchat"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// TestLayoutSnapshots executes production templates without listening on a socket.
// LAYOUT_SHOTS=before|after|fix also saves a portable, offline review bundle.
func TestLayoutSnapshots(t *testing.T) {
	phase := os.Getenv("LAYOUT_SHOTS")
	if phase != "" && phase != "before" && phase != "after" && phase != "fix" {
		t.Fatal("LAYOUT_SHOTS must be before, after or fix")
	}
	bodies := map[string]string{}
	h, f := fullSetup(t)
	f.env.DiskQuota, f.env.DiskUsed = 10<<30, 3<<30
	routes := map[string]string{
		"start": "/", "projects": "/projects", "project": "/projects/report", "project-delete": "/projects/report/delete",
		"trash": "/trash", "trash-delete": "/trash/old--1/delete", "environment": "/environment",
		"environment-starting": "/environment/starting", "environment-reset": "/environment/reset",
		"environment-stop": "/environment/stop", "environment-delete": "/environment/delete", "deleted": "/deleted",
		"usage": "/usage", "usage-method": "/usage/method", "help": "/help", "privacy": "/privacy",
		"how-it-works": "/how-it-works", "consent": "/consent", "consent-declined": "/consent/declined",
		"tools": "/tools", "tools-delete": "/tools/own/mine/delete", "connections": "/connections",
		"onboarding": "/onboarding", "onboarding-2": "/onboarding?step=2",
		"onboarding-3": "/onboarding?step=3", "onboarding-4": "/onboarding?step=4", "settings": "/settings",
		"admin": "/admin", "admin-chats": "/admin/chats", "admin-chat": "/admin/chats/ses_1",
	}
	for name, path := range routes {
		w := do(t, h, req{path: path})
		wantStatus(t, w, 200)
		bodies[name] = w.Body.String()
		if name == "start" || name == "tools" || name == "projects" {
			bodies[name+"-en"] = do(t, h, req{path: path, header: map[string]string{"Accept-Language": "en"}}).Body.String()
		}
	}
	bodies["no-access"] = do(t, h, req{path: "/", id: &Identity{}}).Body.String()
	bodies["error"] = do(t, h, req{path: "/missing-page"}).Body.String()
	for _, state := range []EnvState{EnvStarting, EnvError, EnvStopped} {
		f.env.State = state
		bodies["environment-starting-"+string(state)] = do(t, h, req{path: "/environment/starting"}).Body.String()
	}
	f.projects = map[string]*ProjectDetail{}
	bodies["start-empty"] = do(t, h, req{path: "/"}).Body.String()
	f.trash = nil
	bodies["trash-empty"] = do(t, h, req{path: "/trash"}).Body.String()
	s := &Server{now: time.Now}
	if err := s.parse(os.DirFS("../../web")); err != nil {
		t.Fatal(err)
	}
	extra := map[string]any{
		"teams":                 teamsData{},
		"team":                  teamsData{Team: store.Team{ID: "team", Name: "Exhibition", Role: "admin", Admins: 1, Revision: 1}},
		"knowledge":             knowledgeData{},
		"knowledge-collection":  knowledgeData{Collection: store.KnowledgeCollection{ID: "kb", Title: "Team knowledge", Role: "admin", Writable: true}},
		"knowledge-page":        knowledgeData{Collection: store.KnowledgeCollection{ID: "kb", Title: "Team knowledge", Writable: true}, Page: store.KnowledgePage{ID: "page", Title: "A page", Body: "Text"}},
		"identity-blocked":      nil,
		"admin-assistants":      []assistantEditorData{},
		"admin-assistant":       assistantEditorData{New: true, Fields: assistantFields(assistants.Definition{})},
		"assistant-test":        assistantTestData{Definition: assistants.Definition{ID: "support", Title: "Support"}, History: "[]", AvailableGroups: []string{"staff"}},
		"assistant-gaps":        assistantGapsData{Project: store.Project{Folder: "report"}, CanSubmit: true},
		"admin-assistant-gaps":  []store.AssistantGap{},
		"connection":            connectionData{Connection: Connection{ID: "nextcloud", Title: "Nextcloud", Flow: Flow{State: FlowPending, LoginURL: "https://cloud.example/login"}}},
		"support-chat":          map[string]any{"Project": map[string]any{"Folder": "exhibition-research", "Assistant": map[string]any{"ID": "research", "Version": 2, "Hash": strings.Repeat("a", 64)}}, "Session": map[string]any{"ID": "ses_fixture", "Revision": 1}, "Messages": []map[string]any{{"Role": "user", "Content": "Compare the exhibition visitor numbers."}, {"Role": "assistant", "Content": "The report contains monthly totals. Which months should I compare?"}}, "Evaluation": true, "Prompt": "Compare January and February."},
		"admin-feedback":        adminFeedbackData{Rows: []store.Feedback{{ID: 1, At: time.Now(), Username: "anna", DisplayName: "Anna Beispiel", Kind: "idea", Text: "Keep column headings visible in long reports.", Project: "report"}}},
		"admin-feedback-detail": store.Feedback{ID: 1, At: time.Now(), Username: "anna", DisplayName: "Anna Beispiel", Kind: "idea", Text: "Keep column headings visible in long reports.", Project: "report", Context: "## User\nCompare the visitor numbers.\n## Assistant\nThe report is ready."},
	}
	extra["start-populated"] = startData{
		MaxUpload: 1 << 30, Env: EnvRunning, CapabilitySummary: "Wiki, Web",
		Capabilities: []Tool{{ID: "wiki", Title: "Wiki", On: true}, {ID: "web", Title: "Web", On: true}, {ID: "rooms", Title: "Rooms"}},
		Assistants: []assistantCard{
			{OnStart: true, Choice: assistantchat.Choice{Definition: assistants.Definition{ID: "it-hilfe", Title: "IT-Hilfe", Description: "Hilfe bei IT-Problemen anhand der Anleitungen im Beispiel-Wiki.", EntryQuestion: "Wobei brauchst du Hilfe?", Color: "#28677d", Icon: "tools"}}},
			{OnStart: true, Choice: assistantchat.Choice{Definition: assistants.Definition{ID: "research", Title: "Research", Description: "Literatur und aktuelle Informationen aus dem Bibliothekskatalog und dem Web recherchieren.", EntryQuestion: "Was möchtest du recherchieren?", Color: "#80552e", Icon: "search"}}},
		},
		Projects: []Project{{Name: "besucherzahlen-2025", LastActivity: time.Now().Add(-time.Hour), Status: StatusDone}, {Name: "ausstellungstext", LastActivity: time.Now().Add(-24 * time.Hour), Status: StatusDone}},
	}
	var cards []assistantCard
	for _, def := range instance.Current().Assistants {
		cards = append(cards, assistantCard{Choice: assistantchat.Choice{Definition: def}, Hash: instance.Current().AssistantPin(def).Hash})
	}
	extra["assistants"] = taskAssistantData{Cards: cards}
	for name, data := range extra {
		var b bytes.Buffer
		nav := "projects"
		if name == "assistants" {
			nav = "assistants"
		}
		if strings.HasPrefix(name, "knowledge") {
			nav = "knowledge"
		}
		if name == "teams" || name == "team" {
			nav = "teams"
		}
		if strings.HasPrefix(name, "admin") {
			nav = "admin"
		}
		if strings.HasPrefix(name, "admin-assistant") || name == "assistant-test" {
			nav = "assistant-admin"
		}
		v := view{User: anna, Lang: "de", Nav: nav, Settings: true, Conns: true, Tools: true, Admin: true, TaskAssistants: true, Knowledge: true, KnowledgeWrite: true, D: data}
		page := name
		if name == "start-populated" {
			page = "start"
		}
		if err := s.pages["de"][page+".html"].ExecuteTemplate(&b, "layout", v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		bodies[name] = b.String()
		if name == "assistants" || name == "start-populated" {
			v.Lang = "en"
			enCards := slices.Clone(cards)
			for i := range enCards {
				enCards[i].Title = cmp.Or(enCards[i].TitleEN, enCards[i].Title)
				enCards[i].Description = cmp.Or(enCards[i].DescriptionEN, enCards[i].Description)
				enCards[i].EntryQuestion = cmp.Or(enCards[i].EntryQuestionEN, enCards[i].EntryQuestion)
				enCards[i].OnStart = name == "start-populated"
			}
			if name == "assistants" {
				v.D = taskAssistantData{Cards: enCards}
			} else {
				d := data.(startData)
				d.Assistants = enCards
				v.D = d
			}
			b.Reset()
			if err := s.pages["en"][page+".html"].ExecuteTemplate(&b, "layout", v); err != nil {
				t.Fatal(err)
			}
			bodies[name+"-en"] = b.String()
		}
	}
	// Shared fragments are captured in their actual host pages.
	fh, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web"), Group: UserGroup, Feedback: &fakeFeedback{}})
	if err != nil {
		t.Fatal(err)
	}
	bodies["help-feedback"] = do(t, fh, req{path: "/help"}).Body.String()
	f.watchdog = []WatchdogNotice{{SessionID: "ses_1", Title: "Visitor report", SilentSeconds: 240}}
	bodies["environment-watchdog"] = do(t, h, req{path: "/environment"}).Body.String()
	for _, page := range pageFiles {
		if _, ok := bodies[strings.TrimSuffix(page, ".html")]; !ok {
			t.Errorf("missing fixture: %s", page)
		}
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) { checkLayoutHTML(t, body) })
		if strings.Count(body, "<h1") != 1 {
			t.Errorf("%s: expected one h1", name)
		}
		if strings.Contains(body, `style=`) || strings.Contains(body, "<style") {
			t.Errorf("%s: inline style", name)
		}
	}
	if phase == "" {
		return
	}
	root := os.Getenv("LAYOUT_SHOTS_DIR")
	if root == "" {
		root = "/tmp/layout-shots"
	}
	dir := filepath.Join(root, phase)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	err = fs.WalkDir(os.DirFS("../../web/static"), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, "static", path)
		if d.IsDir() {
			return os.MkdirAll(dst, 0755)
		}
		b, err := os.ReadFile(filepath.Join("../../web/static", path))
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range bodies {
		// Keep app.js enhancements, disable polling/navigation in offline fixtures.
		body = strings.ReplaceAll(body, `"/static/`, `"static/`)
		body = strings.ReplaceAll(body, `<script src="static/js/htmx.min.js" defer></script>`, "")
		body = regexp.MustCompile(`<meta http-equiv="refresh"[^>]*>`).ReplaceAllString(body, "")
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("saved %d pages to %s", len(bodies), dir)
}

// Check rendered output, including conditional form controls and shared fragments.
func checkLayoutHTML(t *testing.T, body string) {
	t.Helper()
	ids, labels := map[string]bool{}, map[string]bool{}
	var fields []map[string]string
	var refs []string
	lastHeading := 0
	z := html.NewTokenizer(strings.NewReader(body))
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if z.Err() != io.EOF {
				t.Fatal(z.Err())
			}
			break
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		attrs := map[string]string{}
		for _, a := range tok.Attr {
			attrs[a.Key] = a.Val
		}
		if id := attrs["id"]; id != "" {
			if ids[id] {
				t.Errorf("duplicate id %q", id)
			}
			ids[id] = true
		}
		if tok.Data == "label" {
			labels[attrs["for"]] = true
		}
		if tok.Data == "input" || tok.Data == "textarea" || tok.Data == "select" {
			if attrs["type"] != "hidden" {
				fields = append(fields, attrs)
			}
		}
		refs = append(refs, strings.Fields(attrs["aria-describedby"]+" "+attrs["aria-labelledby"])...)
		if len(tok.Data) == 2 && tok.Data[0] == 'h' && tok.Data[1] >= '1' && tok.Data[1] <= '6' {
			level := int(tok.Data[1] - '0')
			if level > lastHeading+1 {
				t.Errorf("heading skips from h%d to h%d", lastHeading, level)
			}
			lastHeading = level
		}
	}
	for _, field := range fields {
		if !labels[field["id"]] && field["aria-label"] == "" && field["aria-labelledby"] == "" {
			t.Errorf("unlabelled control: %v", field)
		}
	}
	for _, ref := range refs {
		if !ids[ref] {
			t.Errorf("missing accessible description/label %q", ref)
		}
	}
}

func TestLayoutContrast(t *testing.T) {
	css, err := os.ReadFile("../../web/static/css/app.css")
	if err != nil {
		t.Fatal(err)
	}
	roots := regexp.MustCompile(`(?s):root\s*\{(.*?)\}`).FindAllStringSubmatch(string(css), -1)
	if len(roots) != 3 {
		t.Fatalf("expected light, dark and mobile tokens, got %d", len(roots))
	}
	tokens := regexp.MustCompile(`--([a-z-]+):\s*(#[0-9a-fA-F]{6})`)
	for _, root := range roots[:2] {
		colors := map[string]float64{}
		for _, token := range tokens.FindAllStringSubmatch(root[1], -1) {
			n, err := strconv.ParseUint(token[2][1:], 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			linear := func(v uint64) float64 {
				c := float64(v) / 255
				if c <= 0.04045 {
					return c / 12.92
				}
				return math.Pow((c+0.055)/1.055, 2.4)
			}
			colors[token[1]] = 0.2126*linear(n>>16) + 0.7152*linear((n>>8)&255) + 0.0722*linear(n&255)
		}
		check := func(fg, bg string, minimum float64) {
			t.Helper()
			a, aok := colors[fg]
			b, bok := colors[bg]
			if !aok || !bok {
				t.Fatalf("missing contrast token: %s / %s", fg, bg)
			}
			ratio := (math.Max(a, b) + 0.05) / (math.Min(a, b) + 0.05)
			if ratio < minimum {
				t.Errorf("%s on %s: %.2f:1, need %.1f:1", fg, bg, ratio, minimum)
			}
		}
		for _, bg := range []string{"bg", "surface", "tint", "accent-tint"} {
			for _, fg := range []string{"text", "text-muted", "accent", "danger", "warning", "success", "working"} {
				check(fg, bg, 4.5)
			}
		}
		for _, bg := range []string{"bg", "surface", "tint"} {
			check("border-control", bg, 3)
		}
		check("ink-text", "ink", 4.5)
		check("danger-text", "danger", 4.5)
	}
}
