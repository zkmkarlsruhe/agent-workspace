// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/evaluation"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type fakeFeedback struct {
	mu    sync.Mutex
	sent  []store.Feedback
	reads []string
	limit bool
}

func (f *fakeFeedback) Send(_ context.Context, uid, project, kind, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limit {
		return store.ErrLimit
	}
	f.sent = append(f.sent, store.Feedback{ID: int64(len(f.sent) + 1), At: time.Now(), UID: uid, Source: "portal",
		Project: project, Kind: kind, Text: text, Username: "anna"})
	return nil
}

func (f *fakeFeedback) List(context.Context, int) ([]store.Feedback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent, nil
}

func (f *fakeFeedback) Get(_ context.Context, r evaluation.Reader, id int64) (store.Feedback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, r.Username)
	if id < 1 || int(id) > len(f.sent) {
		return store.Feedback{}, store.ErrNotFound
	}
	fb := f.sent[id-1]
	fb.Context = "\n## Du\n\nes klemmt\n\n## Helfer\n\n- Werkzeug bash: ls (Fehler: kaputt)\n"
	return fb, nil
}

func (f *fakeFeedback) SetDone(_ context.Context, id int64, done bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id < 1 || int(id) > len(f.sent) {
		return store.ErrNotFound
	}
	f.sent[id-1].Done = done
	return nil
}

// The form stores with a checked project; the admin pages are 404 outside
// the admin group, and every detail view is recorded; both languages render.
func TestFeedback(t *testing.T) {
	f := newFake()
	f.addProject("report", "")
	ff := &fakeFeedback{}
	h, err := New(Config{Backend: f, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup,
		Admin: fakeAdmin{}, AdminGroup: "authentik Admins", Feedback: ff})
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, h, req{method: "POST", path: "/feedback", form: url.Values{"text": {" Der Export klemmt "}, "kind": {"idea"}, "project": {"report"}}})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/projects/report?feedback=sent#feedback" {
		t.Fatal(loc)
	}
	w = do(t, h, req{method: "POST", path: "/feedback", form: url.Values{"text": {"x"}, "kind": {"wut"}, "project": {"fremd"}}})
	if loc := w.Header().Get("Location"); loc != "/help?feedback=sent#feedback" {
		t.Fatal(loc)
	}
	if len(ff.sent) != 2 || ff.sent[0].Text != "Der Export klemmt" || ff.sent[0].Project != "report" || ff.sent[0].Kind != "idea" ||
		ff.sent[1].Project != "" || ff.sent[1].Kind != "problem" || ff.sent[0].UID != anna.UID {
		t.Fatalf("%+v", ff.sent)
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/feedback", form: url.Values{"text": {"  "}}}), http.StatusUnprocessableEntity)
	wantStatus(t, do(t, h, req{method: "POST", path: "/feedback", form: url.Values{"text": {strings.Repeat("ä", FeedbackMaxText+1)}}}), http.StatusUnprocessableEntity)
	ff.limit = true
	w = do(t, h, req{method: "POST", path: "/feedback", form: url.Values{"text": {"noch"}}})
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "feedback=limit") {
		t.Fatal(loc)
	}
	ff.limit = false
	wantBody(t, do(t, h, req{path: "/help?feedback=limit"}), "24 Stunden")
	wantBody(t, do(t, h, req{path: "/projects/report?feedback=sent"}), "bei support angekommen", `name="project" value="report"`)

	other := Identity{UID: strings.Repeat("b", 64), Username: "bert", Groups: []string{UserGroup}}
	for _, q := range []req{{path: "/admin/feedback"}, {path: "/admin/feedback/1"}, {method: "POST", path: "/admin/feedback/1/done", form: url.Values{"done": {"1"}}}} {
		q.id = &other
		wantStatus(t, do(t, h, q), http.StatusNotFound)
	}
	if len(ff.reads) != 0 || ff.sent[0].Done {
		t.Fatal("non-admin reached the feedback")
	}
	wantBody(t, do(t, h, req{path: "/admin/feedback"}), "Der Export klemmt", "/admin/feedback/1")
	w = do(t, h, req{path: "/admin/feedback/1"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, "Werkzeug bash: ls (Fehler: kaputt)", "Als erledigt markieren")
	if len(ff.reads) != 1 || ff.reads[0] != "anna" {
		t.Fatalf("read not recorded: %q", ff.reads)
	}
	wantStatus(t, do(t, h, req{path: "/admin/feedback/99"}), http.StatusNotFound)
	wantStatus(t, do(t, h, req{path: "/admin/feedback/x"}), http.StatusNotFound)
	wantStatus(t, do(t, h, req{method: "POST", path: "/admin/feedback/1/done", form: url.Values{"done": {"1"}}}), http.StatusSeeOther)
	if !ff.sent[0].Done {
		t.Fatal("not marked done")
	}

	for _, p := range []string{"/help", "/projects/report", "/admin/feedback", "/admin/feedback/1"} {
		w := do(t, h, req{path: p, header: map[string]string{"Cookie": "sprache=en"}})
		wantStatus(t, w, http.StatusOK)
		if g := germanLeftovers(strings.NewReplacer("Der Export klemmt", "", "es klemmt", "", "kaputt", "").Replace(visibleText(w.Body.String())),
			append(enAllow, "du", "Sie", "Helfer", "Fehler")...); len(g) > 0 { // the excerpt is data, rendered German by lifecycle
			t.Errorf("en %s: German left: %v", p, g)
		}
	}
}
