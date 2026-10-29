// SPDX-License-Identifier: MIT

package portal

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestStartWithoutCapabilities(t *testing.T) {
	s := &Server{}
	if err := s.parse(os.DirFS("../../web")); err != nil {
		t.Fatal(err)
	}
	for _, capabilities := range [][]Tool{nil, {}} {
		var b bytes.Buffer
		if err := s.pages["en"]["start.html"].ExecuteTemplate(&b, "layout", view{User: anna, Lang: "en", D: startData{Capabilities: capabilities}}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), `<details class="disclosure capability-picker"`) {
			t.Fatalf("empty capabilities rendered a picker: %#v", capabilities)
		}
	}
}

type projectTools struct {
	fakeTools
	selected []string
}

func (p *projectTools) ProjectCapabilities(context.Context, string, string) ([]Tool, error) {
	return []Tool{{ID: "workspace-knowledge", Title: "Im Wiki nachsehen", On: true}, {ID: "rooms", Title: "Räume prüfen"}}, nil
}
func (p *projectTools) SelectCapabilities(_ context.Context, _ string, ids []string) ([]string, error) {
	if ids == nil {
		ids = []string{"workspace-knowledge"}
	}
	for _, id := range ids {
		if id != "workspace-knowledge" && id != "rooms" {
			return nil, ErrNotFound
		}
	}
	p.selected = ids
	return ids, nil
}
func (p *projectTools) SetProjectCapability(context.Context, string, string, string, bool) (ToolEffect, error) {
	return EffectBusy, nil
}

func TestProjectCapabilityForms(t *testing.T) {
	f, tools := newFake(), &projectTools{}
	f.suggest = "research"
	h, err := New(Config{Backend: f, Tools: tools, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, h, req{path: "/"})
	wantStatus(t, w, 200)
	wantBody(t, w, "Was möchtest du erledigen?", "Fähigkeiten für dieses Projekt", "Vorausgewählt: Im Wiki nachsehen", `<details class="disclosure capability-picker" id="capability">`, `id="cap-workspace-knowledge"`, `value="workspace-knowledge" checked`, `value="rooms">`)
	if strings.Index(w.Body.String(), `name="capability"`) > strings.Index(w.Body.String(), `name="files"`) {
		t.Fatal("capabilities must precede streaming uploads")
	}
	for _, ids := range [][]string{{"workspace-knowledge"}, {}} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		mw.WriteField("description", "Bitte recherchiere das Thema.")
		mw.WriteField("capabilities_present", "1")
		for _, id := range ids {
			mw.WriteField("capability", id)
		}
		part, err := mw.CreateFormFile("files", "note.txt")
		if err != nil {
			t.Fatal(err)
		}
		part.Write([]byte("source"))
		mw.Close()
		wantStatus(t, do(t, h, req{method: "POST", path: "/projects/start", body: &body, ctype: mw.FormDataContentType()}), 303)
		created := f.created[len(f.created)-1]
		if created.Capabilities == nil || !slices.Equal(created.Capabilities, ids) {
			t.Fatal(created)
		}
	}
	// Invalid descriptions retain the person's selection, rather than restoring defaults.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("description", "short")
	mw.WriteField("capabilities_present", "1")
	mw.WriteField("capability", "rooms")
	mw.Close()
	w = do(t, h, req{method: "POST", path: "/projects/start", body: &body, ctype: mw.FormDataContentType()})
	wantStatus(t, w, 422)
	wantBody(t, w, `value="rooms" checked`, `value="workspace-knowledge">`, "Vorausgewählt: Räume prüfen")
	body.Reset()
	mw = multipart.NewWriter(&body)
	mw.WriteField("description", "short")
	mw.WriteField("capabilities_present", "1")
	mw.Close()
	w = do(t, h, req{method: "POST", path: "/projects/start", body: &body, ctype: mw.FormDataContentType()})
	wantStatus(t, w, 422)
	wantBody(t, w, "Vorausgewählt: Keine zusätzlichen Fähigkeiten", `value="rooms">`, `value="workspace-knowledge">`)
	w = do(t, h, req{path: "/projects/research"})
	wantBody(t, w, "Fähigkeiten in diesem Projekt", `aria-pressed="true"`)
	w = do(t, h, req{method: "POST", path: "/projects/research/capabilities", form: url.Values{"capability": {"rooms"}, "on": {"1"}}})
	wantStatus(t, w, http.StatusSeeOther)
	if !strings.Contains(w.Header().Get("Location"), "effect=restart") {
		t.Fatal(w.Header())
	}
	def, ok := instance.Current().Assistant("it-hilfe")
	if !ok || def.ProjectMode != "shared" {
		t.Fatal("shared assistant missing")
	}
	f.projects["research"].Assistant = instance.Current().AssistantPin(def)
	w = do(t, h, req{path: "/projects/research"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, `id="results"`, `id="inbox"`)
	if strings.Contains(w.Body.String(), `action="/projects/research/rename"`) {
		t.Fatal("shared assistant folder is mutable")
	}
	if strings.Contains(w.Body.String(), "Fähigkeiten in diesem Projekt") {
		t.Fatal("assistant has toggles")
	}
}

func TestStartCapabilityPartLimit(t *testing.T) {
	for _, count := range []int{64, 65} {
		for _, field := range []string{"capability", "capabilities_present"} {
			f := newFake()
			f.suggest = "research"
			h, err := New(Config{Backend: f, Tools: &projectTools{}, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org"})
			if err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			mw.WriteField("description", "Bitte recherchiere das Thema.")
			for range count - 1 {
				mw.WriteField(field, "rooms")
			}
			mw.Close()
			w := do(t, h, req{method: "POST", path: "/projects/start", body: &body, ctype: mw.FormDataContentType()})
			want := http.StatusSeeOther
			if count > 64 {
				want = http.StatusBadRequest
			}
			wantStatus(t, w, want)
			if count > 64 && len(f.created) != 0 {
				t.Fatal("created despite excessive parts")
			}
		}
	}
}
