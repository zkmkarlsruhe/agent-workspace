// SPDX-License-Identifier: MIT

package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func knowledgePortal(t *testing.T) (http.Handler, *store.Store, store.KnowledgeActor, Identity, store.KnowledgeActor) {
	t.Helper()
	st, e := store.Open(filepath.Join(t.TempDir(), "knowledge.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	bob := Identity{UID: strings.Repeat("b", 64), Username: "bob", Name: "Bob", Groups: []string{UserGroup}}
	ctx := context.Background()
	for _, id := range []Identity{anna, bob} {
		if _, e = st.EnsureUser(ctx, identity.Identity{UID: id.UID, Username: id.Username, Name: id.Name}); e != nil {
			t.Fatal(e)
		}
		if e = st.AcceptConsent(ctx, id.UID, consent.Version(), time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	a, e := st.KnowledgeActor(ctx, anna.UID)
	if e != nil {
		t.Fatal(e)
	}
	b, e := st.KnowledgeActor(ctx, bob.UID)
	if e != nil {
		t.Fatal(e)
	}
	h, e := New(Config{Backend: newFake(), Knowledge: st, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup, AdminGroup: "authentik Admins"})
	if e != nil {
		t.Fatal(e)
	}
	return h, st, a, bob, b
}
func TestKnowledgePortalFormsConflictAndPrivacy(t *testing.T) {
	h, st, a, bob, _ := knowledgePortal(t)
	ctx := context.Background()
	for _, path := range []string{"/teams", "/knowledge", "/teams-recovery"} {
		for _, lang := range []string{"de", "en"} {
			w := do(t, h, req{path: path, header: map[string]string{"Accept-Language": lang}})
			wantStatus(t, w, 200)
			checkLayoutHTML(t, w.Body.String())
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cache")
			}
		}
	}
	form := url.Values{"account": {a.Stamp}, "name": {"Museum <script>"}, "description": {"Team purpose"}}
	wantStatus(t, do(t, h, req{method: "POST", path: "/teams", form: form, header: map[string]string{"Origin": "https://foreign.test"}}), 403)
	w := do(t, h, req{method: "POST", path: "/teams", form: form})
	wantStatus(t, w, 303)
	teamURL := w.Header().Get("Location")
	w = do(t, h, req{path: teamURL})
	wantStatus(t, w, 200)
	wantBody(t, w, "Museum &lt;script&gt;")
	checkLayoutHTML(t, w.Body.String())
	wantStatus(t, do(t, h, req{path: teamURL, id: &bob}), 404)
	wantStatus(t, do(t, h, req{path: "/teams-recovery", id: &bob}), 404)
	c, e := st.CreateKnowledgeCollection(ctx, a, "", "Private", "Hidden title")
	if e != nil {
		t.Fatal(e)
	}
	path := knowledgeURL(c.ID)
	w = do(t, h, req{path: path})
	wantStatus(t, w, 200)
	checkLayoutHTML(t, w.Body.String())
	w = do(t, h, req{path: path + "/pages/new"})
	wantStatus(t, w, 200)
	checkLayoutHTML(t, w.Body.String())
	pageURL := knowledgePageURL(c.ID, uuid.NewString())
	draft := url.Values{"account": {a.Stamp}, "revision": {"0"}, "title": {"First"}, "body": {"# Content\n<script>alert(1)</script>\n[bad](javascript:alert(1))\n![tracking](https://tracker.test/x)"}, "sources": {"Original source"}}
	wantStatus(t, do(t, h, req{method: "POST", path: pageURL, form: draft}), 303)
	w = do(t, h, req{path: pageURL})
	wantStatus(t, w, 200)
	checkLayoutHTML(t, w.Body.String())
	if strings.Contains(w.Body.String(), "<script>alert") || strings.Contains(w.Body.String(), `src="https://tracker.test`) || strings.Contains(w.Body.String(), `href="javascript:`) {
		t.Fatal("unsafe preview")
	}
	draft.Set("revision", "1")
	draft.Set("body", "Saved revision two")
	wantStatus(t, do(t, h, req{method: "POST", path: pageURL, form: draft}), 303)
	draft.Set("body", "My unsaved draft")
	w = do(t, h, req{method: "POST", path: pageURL, form: draft})
	wantStatus(t, w, 409)
	wantBody(t, w, "My unsaved draft", `name="current_revision" value="2"`)
	checkLayoutHTML(t, w.Body.String())
	draft.Set("reviewed", "true")
	draft.Set("current_revision", "2")
	wantStatus(t, do(t, h, req{method: "POST", path: pageURL, form: draft}), 303)
	for _, foreign := range []string{path, path + "/export", pageURL, pageURL + "?revision=1"} {
		wantStatus(t, do(t, h, req{path: foreign, id: &bob}), 404)
	}
	draft.Set("account", "wrong")
	wantStatus(t, do(t, h, req{method: "POST", path: pageURL, form: draft}), 404)
	w = do(t, h, req{path: path + "/export"})
	wantStatus(t, w, 200)
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("inline export")
	}
	hOff, _ := setup(t)
	for _, p := range []string{"/teams", "/knowledge"} {
		wantStatus(t, do(t, hOff, req{path: p}), 404)
	}
}
func TestTeamPortalMemberLifecycleAndRevocation(t *testing.T) {
	h, st, a, bob, b := knowledgePortal(t)
	ctx := context.Background()
	team, e := st.CreateTeam(ctx, a, "Project", "")
	if e != nil {
		t.Fatal(e)
	}
	candidates, e := st.TeamCandidates(ctx, a, team.ID, "bob")
	if e != nil || len(candidates) != 1 {
		t.Fatal(candidates, e)
	}
	v := url.Values{"account": {a.Stamp}, "revision": {"1"}, "member": {b.UID}, "member_stamp": {candidates[0].Stamp}, "role": {"member"}}
	wantStatus(t, do(t, h, req{method: "POST", path: "/teams/" + team.ID + "/members", form: v}), 303)
	wantStatus(t, do(t, h, req{method: "POST", path: "/teams/" + team.ID + "/invitation", id: &bob, form: url.Values{"account": {b.Stamp}, "revision": {"2"}, "decision": {"accept"}}}), 303)
	c, e := st.CreateKnowledgeCollection(ctx, a, team.ID, "Shared", "")
	if e != nil {
		t.Fatal(e)
	}
	p, e := st.SaveKnowledgePage(ctx, b, store.KnowledgePage{ID: uuid.NewString(), CollectionID: c.ID, Title: "Shared page", Body: "Team text"}, 0)
	if e != nil {
		t.Fatal(e)
	}
	wantStatus(t, do(t, h, req{path: knowledgePageURL(c.ID, p.ID), id: &bob}), 200)
	wantStatus(t, do(t, h, req{path: knowledgeURL(c.ID) + "/export", id: &bob}), 404)
	malicious := url.Values{"account": {b.Stamp}, "revision": {"2"}, "member": {b.UID}, "role": {"admin"}}
	wantStatus(t, do(t, h, req{method: "POST", path: "/teams/" + team.ID + "/members", form: malicious, id: &bob}), 404)
	v.Set("revision", "3")
	v.Set("role", "")
	wantStatus(t, do(t, h, req{method: "POST", path: "/teams/" + team.ID + "/members", form: v}), 303)
	wantStatus(t, do(t, h, req{path: knowledgePageURL(c.ID, p.ID), id: &bob}), 404)
	w := do(t, h, req{path: "/knowledge?q=Team", id: &bob})
	wantStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "Shared page") {
		t.Fatal("revoked search")
	}
}

func TestKnowledgeDisabledRetainsExportAndCleanup(t *testing.T) {
	_, st, a, _, _ := knowledgePortal(t)
	ctx := context.Background()
	c, e := st.CreateKnowledgeCollection(ctx, a, "", "Existing", "")
	if e != nil {
		t.Fatal(e)
	}
	h, e := New(Config{Backend: newFake(), Knowledge: st, KnowledgeReadOnly: true, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup, AdminGroup: "authentik Admins"})
	if e != nil {
		t.Fatal(e)
	}
	w := do(t, h, req{path: "/knowledge"})
	wantStatus(t, w, 200)
	if strings.Contains(w.Body.String(), `action="/knowledge/import"`) {
		t.Fatal("import offered while disabled")
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/knowledge", form: url.Values{"account": {a.Stamp}, "title": {"New"}}}), 409)
	wantStatus(t, do(t, h, req{path: knowledgeURL(c.ID) + "/export"}), 200)
	wantStatus(t, do(t, h, req{method: "POST", path: knowledgeURL(c.ID), form: url.Values{"account": {a.Stamp}, "revision": {"1"}, "title": {c.Title}, "archived": {"true"}}}), 303)
	wantStatus(t, do(t, h, req{method: "POST", path: knowledgeURL(c.ID) + "/purge", form: url.Values{"account": {a.Stamp}, "revision": {"2"}, "confirm": {"purge"}}}), 303)
}

func TestKnowledgeMarkdownAndPortableImport(t *testing.T) {
	h, st, a, _, _ := knowledgePortal(t)
	ctx := context.Background()
	c, e := st.CreateKnowledgeCollection(ctx, a, "", "Imports", "")
	if e != nil {
		t.Fatal(e)
	}
	page := uuid.NewString()
	upload := func(path, field, filename string, data []byte, fields map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range fields {
			if e := mw.WriteField(k, v); e != nil {
				t.Fatal(e)
			}
		}
		f, e := mw.CreateFormFile(field, filename)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(data); e != nil {
			t.Fatal(e)
		}
		if e = mw.Close(); e != nil {
			t.Fatal(e)
		}
		return do(t, h, req{method: "POST", path: path, body: &body, ctype: mw.FormDataContentType()})
	}

	t.Run("import_body_limit", func(t *testing.T) {
		w := upload("/knowledge/import", "manifest", "manifest.json", bytes.Repeat([]byte(" "), (20<<20)+1), map[string]string{"account": a.Stamp})
		wantStatus(t, w, http.StatusRequestEntityTooLarge)
		wantBody(t, w, "20 MiB")
	})
	fields := map[string]string{"account": a.Stamp, "title": "Imported", "page": page, "sources": "https://example.org/source"}
	for range 2 {
		wantStatus(t, upload(knowledgeURL(c.ID)+"/markdown", "markdown", "note.md", []byte("# Draft\nEvidence"), fields), 303)
	}
	rows, e := st.KnowledgePages(ctx, a, c.ID)
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	w := do(t, h, req{path: knowledgePageURL(c.ID, page) + "/download?revision=1"})
	wantStatus(t, w, 200)
	wantBody(t, w, "Evidence", "https://example.org/source")
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("inline markdown")
	}
	bundle, e := st.ExportKnowledge(ctx, a, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	data, e := json.Marshal(bundle)
	if e != nil {
		t.Fatal(e)
	}
	w = upload("/knowledge/import", "manifest", "manifest.json", data, map[string]string{"account": a.Stamp})
	wantStatus(t, w, 303)
	imported := strings.TrimPrefix(w.Header().Get("Location"), "/knowledge/")
	pages, e := st.KnowledgePages(ctx, a, imported)
	if e != nil || len(pages) != 1 || pages[0].ID == page {
		t.Fatal(pages, e)
	}
	fields["account"] = "stale"
	wantStatus(t, upload(knowledgeURL(c.ID)+"/markdown", "markdown", "note.md", []byte("bad"), fields), 404)
}
