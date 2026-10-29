// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistantchat"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestAssistantCardsAndSubmit(t *testing.T) {
	ctx := context.Background()
	d, err := testinstance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Select(d)()
	d.Assistants[0].ProjectMode = "per_task"
	d.Assistants[0].EntryQuestion = "Question <script> & help?"
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.EnsureUser(ctx, identity.Identity{UID: anna.UID, Username: anna.Username}); err != nil {
		t.Fatal(err)
	}
	s := &assistantchat.Service{Store: st}
	cat := &catalog.Service{Dir: "../../../deploy/catalog", Store: st, Log: slog.New(slog.DiscardHandler)}
	s.Items = cat.AssistantItems
	s.Tools = func(context.Context, store.User, func() (assistants.Pin, error)) ([]*sdk.Tool, error) {
		return nil, nil
	}
	s.Call = func(context.Context, store.User, string, string, json.RawMessage, func() (assistants.Pin, error)) (*sdk.CallToolResult, error) {
		t.Fatal("unexpected tool")
		return nil, nil
	}
	s.Complete = func(context.Context, store.User, string, string, int, any, any) (json.RawMessage, error) {
		t.Fatal("legacy executor called")
		return nil, nil
	}
	f := newFake()
	h, err := New(Config{Backend: f, TaskAssistants: s, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(t, do(t, h, req{path: "/new?profile=it-hilfe", id: &Identity{}}), 403)
	for _, path := range []string{"/assistants?assistant=it-hilfe", "/new?assistant=it-hilfe", "/new?profile=it-hilfe", "/new?assistant=it-hilfe&profile=unknown"} {
		w := do(t, h, req{path: path})
		wantStatus(t, w, 200)
		wantBody(t, w, `id="assistant-it-hilfe"`, `name="assistant"`, "autofocus")
	}
	wantStatus(t, do(t, h, req{path: "/new?assistant=unknown&profile=it-hilfe"}), 403)
	w := do(t, h, req{path: "/assistants?assistant=it-hilfe"})
	wantStatus(t, w, 200)
	wantBody(t, w, "IT-Hilfe", "Assistenten", `href="/assistants" aria-current="page"`, `name="question"`, "autofocus", "Question &lt;script&gt; &amp; help?", d.AssistantPin(d.Assistants[0]).Hash)
	wantBody(t, w, `<textarea class="textarea" id="question-it-hilfe" name="question" rows="4" required maxlength="16000"`,
		`<label class="field__label" for="question-it-hilfe">Question &lt;script&gt; &amp; help?</label>`, `data-submit-shortcut aria-describedby="question-help-it-hilfe"`,
		`id="question-help-it-hilfe"`, "Strg+Enter oder Cmd+Enter", ">Starten</button>")
	if strings.Contains(w.Body.String(), "disabled") {
		t.Fatal("fresh account profile card disabled", w.Body)
	}
	for _, language := range []string{"de", "en"} {
		for _, path := range []string{"/", "/assistants"} {
			page := do(t, h, req{path: path, header: map[string]string{"Accept-Language": language}})
			wantStatus(t, page, 200)
			wantBody(t, page, `class="assistant-card__accent"`, `fill="#28677d"`, `viewBox="0 0 24 24"`,
				`action="/new"`, d.AssistantPin(d.Assistants[0]).Hash)
			checkLayoutHTML(t, page.Body.String())
			if language == "en" {
				wantBody(t, page, "IT support", "using the instructions in the Workspace wiki", "What do you need help with?")
			}
			if strings.Contains(page.Body.String(), "style=") || strings.Contains(page.Body.String(), "Choose an assistant") {
				t.Fatal("inline styling or duplicate chooser", page.Body)
			}
		}
	}
	if ids, err := st.OptIns(ctx, anna.UID); err != nil || len(ids) != 0 {
		t.Fatal("card changed personal catalog", ids, err)
	}
	if len(f.created) != 0 || len(f.sessions) != 0 {
		t.Fatal("GET started a project")
	}
	question := "  Mein Drucker: Fehler \"E42\" — äöü?\r\n\r\n\tWas tun?\n  "
	form := url.Values{"assistant": {"it-hilfe"}, "profile": {"unknown"}, "hash": {d.AssistantPin(d.Assistants[0]).Hash}, "question": {question}}
	wantStatus(t, do(t, h, req{method: "POST", path: "/new", form: form}), 403)
	if err := st.AcceptConsent(ctx, anna.UID, consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, headers := range []map[string]string{{"Origin": "https://other.example"}, {"Sec-Fetch-Site": "same-site"}, {"Sec-Fetch-Site": "cross-site"}} {
		wantStatus(t, do(t, h, req{method: "POST", path: "/new", form: form, header: headers}), 403)
	}
	form.Set("hash", "stale")
	wantStatus(t, do(t, h, req{method: "POST", path: "/new", form: form}), 403)
	form.Set("hash", d.AssistantPin(d.Assistants[0]).Hash)
	form.Set("question", "   ")
	wantStatus(t, do(t, h, req{method: "POST", path: "/new", form: form}), 422)
	form.Set("question", question)
	if len(f.created) != 0 || len(f.calls) != 0 {
		t.Fatal("rejected form did work")
	}
	for _, failure := range []bool{false, true} {
		f.suggest, f.suggErr = "drucker-hilfe", nil
		if failure {
			form.Set("profile", form.Get("assistant"))
			form.Del("assistant")
			f.suggErr = errors.New("fast model unavailable")
		}
		w = do(t, h, req{method: "POST", path: "/new", form: form})
		wantStatus(t, w, 303)
		if !strings.HasPrefix(w.Header().Get("Location"), "https://anna.apps.example.org/server/xyz/session/ses_") {
			t.Fatal("not OpenCode", w.Header())
		}
		created := f.created[len(f.created)-1]
		if created.Description != question || f.projects[created.Name].Assistant.ID != "it-hilfe" {
			t.Fatal("profile/question lost", created)
		}
		if !strings.HasSuffix(f.sessions[len(f.sessions)-1], assistantPrompt(d.Assistants[0].InitPrompt, question)) {
			t.Fatal("first prompt changed question", f.sessions)
		}
		if failure && !strings.HasSuffix(created.Name, "mein-drucker-fehler") {
			t.Fatal("wrong fallback", created.Name)
		}
	}
	if len(f.calls) != 2 || len(f.created) != 2 || len(f.sessions) != 2 {
		t.Fatal("not exactly one preparation per submit", f.calls)
	}
	// Shared submissions ignore generated names, including when preparation fails.
	d.Assistants[0].ProjectMode = "shared"
	form.Set("hash", d.AssistantPin(d.Assistants[0]).Hash)
	before := len(f.created)
	for _, failure := range []bool{false, true} {
		f.suggErr = nil
		if failure {
			f.suggErr = errors.New("model unavailable")
		}
		wantStatus(t, do(t, h, req{method: "POST", path: "/new", form: form}), 303)
	}
	if len(f.created) != before+1 || f.created[before].Name != "it-hilfe" || len(f.sessions) != 4 {
		t.Fatal("shared submissions did not reuse project", f.created, f.sessions)
	}
	w = do(t, h, req{path: "/projects/it-hilfe"})
	wantStatus(t, w, 200)
	wantBody(t, w, `id="results"`)
	if strings.Contains(w.Body.String(), `class="disclosure rename"`) {
		t.Fatal("shared name editable")
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/projects/it-hilfe/rename", form: url.Values{"new_name": {"renamed"}}}), 409)
	// Missing sources disable the card and deny a forged POST.
	s.Items = func(assistants.Definition) []catalog.Item { return nil }
	w = do(t, h, req{path: "/new"})
	wantBody(t, w, "Benötigte Wissensquelle nicht verfügbar", "disabled")
	wantBody(t, do(t, h, req{path: "/"}), "Benötigte Wissensquelle nicht verfügbar", "disabled")
	wantStatus(t, do(t, h, req{method: "POST", path: "/new", form: form}), 403)
	d.Assistants[0].EntryQuestion = ""
	d.Assistants[0].EntryQuestionEN = ""
	w = do(t, h, req{path: "/new"})
	wantBody(t, w, "Wobei kann ich dir helfen?")
	w = do(t, h, req{path: "/new", header: map[string]string{"Accept-Language": "en"}})
	wantBody(t, w, "How can I help you?")
	d.Assistants[0].Audience = []string{"not-my-group"}
	w = do(t, h, req{path: "/new"})
	wantStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "IT-Hilfe") || strings.Contains(do(t, h, req{path: "/"}).Body.String(), `id="assistant-it-hilfe"`) {
		t.Fatal("forbidden card rendered")
	}
	wantStatus(t, do(t, h, req{path: "/new?profile=it-hilfe"}), 403)
	wantStatus(t, do(t, h, req{method: "POST", path: "/new", form: form}), 403)
	wantStatus(t, do(t, h, req{method: "POST", path: "/support/ses_old/start-question", form: url.Values{"index": {"0"}}}), 404)
}

func TestIsolatedAssistantProjectRename(t *testing.T) {
	h, f := setup(t)
	def, ok := instance.Current().Assistant("it-hilfe")
	if !ok || def.ProjectMode != "shared" {
		t.Fatal("expected shared assistant definition")
	}
	f.addProject("legacy-support", "Private support")
	p := f.projects["legacy-support"]
	p.Isolated = true
	p.Assistant = instance.Current().AssistantPin(def)
	w := do(t, h, req{path: "/projects/legacy-support"})
	wantStatus(t, w, 200)
	wantBody(t, w, `class="disclosure rename"`)
	w = do(t, h, req{method: "POST", path: "/projects/legacy-support/rename", form: url.Values{"new_name": {"renamed"}}})
	wantStatus(t, w, 303)
	if got := f.projects["renamed"]; got == nil || !got.Isolated || got.Assistant != p.Assistant {
		t.Fatal("isolated project binding lost", got)
	}
	if len(f.calls) != 0 || len(f.sessions) != 0 {
		t.Fatal("rename started runtime work", f.calls, f.sessions)
	}
}

func TestSupportSendHidesStoreError(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "support.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.EnsureUser(ctx, identity.Identity{UID: anna.UID, Username: anna.Username}); err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptConsent(ctx, anna.UID, consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	def := instance.Current().Assistants[0]
	p := store.Project{ID: "support", UID: anna.UID, Folder: "support", CreatedAt: time.Now(), Assistant: instance.Current().AssistantPin(def)}
	if err := st.CreateAssistantProject(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAssistantSession(ctx, anna.UID, p.ID, "ses_support", time.Now()); err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Service{Dir: "../../../deploy/catalog", Store: st, Log: slog.New(slog.DiscardHandler)}
	service := &assistantchat.Service{Store: st, Items: cat.AssistantItems}
	var storeErr error
	service.Tools = func(ctx context.Context, _ store.User, _ func() (assistants.Pin, error)) ([]*sdk.Tool, error) {
		st.Close()
		_, storeErr = st.UserByUID(ctx, anna.UID)
		return nil, storeErr
	}
	service.Call = func(context.Context, store.User, string, string, json.RawMessage, func() (assistants.Pin, error)) (*sdk.CallToolResult, error) {
		t.Fatal("unexpected call")
		return nil, nil
	}
	service.Complete = func(context.Context, store.User, string, string, int, any, any) (json.RawMessage, error) {
		t.Fatal("unexpected completion")
		return nil, nil
	}
	h, err := New(Config{Backend: newFake(), TaskAssistants: service, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup})
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, h, req{method: "POST", path: "/support/ses_support", form: url.Values{"revision": {"0"}, "message": {"Help with the printer"}}})
	wantStatus(t, w, 500)
	if storeErr == nil {
		t.Fatal("store failure not exercised")
	}
	if strings.Contains(w.Body.String(), storeErr.Error()) {
		t.Fatal("internal error exposed", w.Body.String())
	}
}
