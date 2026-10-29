// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// catalogFor wires the catalog like run() does, on a temp copy of
// deploy/catalog; the container paths of the scripts point to temp dirs
// (standard skills: the real deploy/etc-opencode/skills, read only).
func catalogFor(t testing.TB, rt *lctest.Runtime, svc *lifecycle.Service, st *store.Store, log *slog.Logger) *catalog.Service {
	root := t.TempDir()
	dir, home := filepath.Join(root, "catalog"), filepath.Join(root, "skills")
	if err := os.CopyFS(dir, os.DirFS("../../../deploy/catalog")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(home, 0o755)
	std, _ := filepath.Abs("../../../deploy/etc-opencode/skills")
	rt.Paths = map[string]string{catalog.SkillsDir: home, catalog.MountDir(): dir, catalog.StandardDir: std, catalog.ConfigFile(): filepath.Join(root, "catalog.json")}
	cat := &catalog.Service{Dir: dir, Store: st, Shell: svc.Shell, Client: svc.Client, Running: svc.RunningUIDs, Log: log}
	svc.AfterStart, svc.AfterReap, portalTools = cat.AfterStart, cat.RetryPending, tools{cat}
	return cat
}

// seedTools fills the Tools page of the render harness.
func seedTools(t testing.TB, s *stack) {
	ctx := context.Background()
	// the catalog skills are the real ones from deploy/catalog (catalogFor)
	if _, err := s.cat.Set(ctx, alice.uid, "event-planning", true); err != nil {
		t.Fatal(err)
	}
	home := s.rt.Paths[catalog.SkillsDir]
	for id, desc := range map[string]string{
		"meine-besucherzahlen": "Besucherzahlen aus dem Kassensystem nach Monat und Ausstellung auswerten.",
		"meine-pressetexte":    "Pressetexte im Ton der Workspace-Kommunikation, 2000 Zeichen, mit Bildnachweis.",
	} {
		os.MkdirAll(filepath.Join(home, id), 0o755)
		os.WriteFile(filepath.Join(home, id, "SKILL.md"), []byte("---\nname: "+id+"\ndescription: "+desc+"\n---\n"), 0o644)
	}
}

// addCatalogSkill puts a valid catalog skill into the stack's catalog copy.
func addCatalogSkill(t testing.TB, s *stack, id, title, category, desc string) {
	d := filepath.Join(s.cat.Dir, "skills", id)
	os.MkdirAll(d, 0o755)
	fm := "---\nname: " + id + "\ndescription: \"" + desc + "\"\nmetadata:\n  workspace/version: \"1.0.0\"\n  workspace/owner: \"operations\"\n" +
		"  workspace/license: \"MIT\"\n  workspace/tier: \"optional\"\n  workspace/title: \"" + title + "\"\n  workspace/title-de: \"" + title + "\"\n  workspace/category: \"" + category + "\"\n---\n# " + title + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestToolsE2E(t *testing.T) {
	s := newStack(t, "127.83")
	me := &alice
	ctx := context.Background()
	home := s.rt.Paths[catalog.SkillsDir]
	addCatalogSkill(t, s, "minutes", "Protokolle schreiben", "texts", "Sitzungsprotokolle aus Notizen.")
	// the switch logic is tested on an opt-in MCP item; in deploy/catalog md-to-pdf is standard
	mcp := filepath.Join(s.cat.Dir, "mcp.json")
	b, _ := os.ReadFile(mcp)
	// md-to-pdf is the only standard item in category texts
	std := `"tier": "standard",` + "\n" + `      "category": "texts"`
	if !strings.Contains(string(b), std) {
		t.Fatal("deploy/catalog/mcp.json changed: md-to-pdf not found")
	}
	os.WriteFile(mcp, []byte(strings.Replace(string(b), std, `"tier": "optional",`+"\n"+`      "category": "texts"`, 1)), 0o644)

	// asleep: catalog and standard MCP items, no skills from the container
	want(t, s.do(t, call{url: "/tools", as: me}), http.StatusOK, "Dein Arbeitsbereich ist ausgeschaltet", "Im Web suchen und lesen",
		"Ein PDF erstellen", "Protokolle schreiben", `href="/tools" aria-current="page"`)
	u, _ := s.st.UserByUID(ctx, alice.uid)
	if allowed, off := s.cat.Grants(ctx, u.UID, ""); allowed["md_to_pdf"] || off["md_to_pdf"] == "" || !allowed["web_search"] {
		t.Fatal("md_to_pdf must be hidden before it is switched on")
	}
	// a switch while asleep is stored and applied by the start hook
	wantLoc(t, s.do(t, call{method: "POST", url: "/tools/minutes", form: url.Values{"on": {"1"}}, as: me}), "/tools?toggled=minutes&effect=next-start")
	if _, err := os.Lstat(filepath.Join(home, "minutes")); err == nil {
		t.Fatal("link created without a running environment")
	}
	if _, err := s.svc.EnsureRunning(ctx, u.UID); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(home, "minutes")); err != nil || target != filepath.Join(s.cat.Dir, "skills", "minutes") {
		t.Fatalf("link after start: %q %v", target, err)
	}
	reloads := s.oc.Reloads

	// running: standard skills from the container, own skills, switching reloads
	os.MkdirAll(filepath.Join(home, "meine-besucher"), 0o755)
	os.WriteFile(filepath.Join(home, "meine-besucher", "SKILL.md"), []byte("---\nname: meine-besucher\ndescription: Besucherzahlen nach Monat.\n---\n"), 0o644)
	w := s.do(t, call{url: "/tools", as: me})
	want(t, w, http.StatusOK, "Immer dabei", "Excel-Tabellen", "Word-Dokumente", "Gespeicherte Anleitungen", "meine-besucher", "Besucherzahlen nach Monat.")
	if strings.Contains(w.Body.String(), "Dein Arbeitsbereich ist ausgeschaltet") || strings.Count(w.Body.String(), "<h1") != 1 {
		t.Fatal("running page")
	}
	wantLoc(t, s.do(t, call{method: "POST", url: "/tools/md-to-pdf", form: url.Values{"on": {"1"}}, as: me}), "/tools?toggled=md-to-pdf&effect=now")
	if allowed, _ := s.cat.Grants(ctx, u.UID, ""); s.oc.Reloads != reloads+1 || !allowed["md_to_pdf"] {
		t.Fatalf("reloads %d, allowed %v", s.oc.Reloads, allowed)
	}
	s.oc.Mu.Lock()
	s.oc.Active = []string{"ses_1"}
	s.oc.Mu.Unlock()
	wantLoc(t, s.do(t, call{method: "POST", url: "/tools/minutes", form: url.Values{"on": {"0"}}, as: me}), "/tools?toggled=minutes&effect=restart")
	if _, err := os.Lstat(filepath.Join(home, "minutes")); err == nil || s.oc.Reloads != reloads+1 {
		t.Fatalf("link must go, no reload while busy: %v reloads %d", err, s.oc.Reloads)
	}
	s.oc.Mu.Lock()
	s.oc.Active = nil
	s.oc.Mu.Unlock()
	// the busy switch is owed a reload: the reap tick brings it
	if s.cat.RetryPending(ctx); s.oc.Reloads != reloads+2 {
		t.Fatalf("busy switch not retried: reloads %d", s.oc.Reloads)
	}
	want(t, s.do(t, call{method: "POST", url: "/tools/web-research", form: url.Values{"on": {"0"}}, as: me}), http.StatusNotFound)

	// own skill: confirmation page, then gone
	want(t, s.do(t, call{url: "/tools/own/meine-besucher/delete", as: me}), http.StatusOK, "Diese Anleitung löschen?")
	wantLoc(t, s.do(t, call{method: "POST", url: "/tools/own/meine-besucher/delete", form: url.Values{"confirmed": {"yes"}}, as: me}), "/tools?deleted=meine-besucher")
	if _, err := os.Stat(filepath.Join(home, "meine-besucher")); err == nil {
		t.Fatal("own skill still there")
	}
	want(t, s.do(t, call{url: "/tools/own/minutes/delete", as: me}), http.StatusNotFound)
}

func TestToolsMissingLocationE2E(t *testing.T) {
	s := newStack(t, "127.83")
	ctx := context.Background()
	s.do(t, call{url: "/tools", as: &alice})
	u, err := s.st.UserByUID(ctx, alice.uid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.EnsureRunning(ctx, u.UID); err != nil {
		t.Fatal(err)
	}
	// The shell probe passes; the fake loses this directory during reload.
	gone := t.TempDir()
	s.oc.Mu.Lock()
	s.oc.Locations = []ocapi.Location{{Directory: gone}, {Directory: "/"}}
	s.oc.MissingDirs = map[string]bool{gone: true}
	s.oc.Mu.Unlock()
	wantLoc(t, s.do(t, call{method: "POST", url: "/tools/rooms", form: url.Values{"on": {"1"}}, as: &alice}), "/tools?toggled=rooms&effect=restart")
	on, err := s.cat.On(ctx, u.UID)
	if err != nil || !on["rooms"] {
		t.Fatal("toggle was not saved", err)
	}
	s.cat.RetryPending(ctx)
	s.oc.Mu.Lock()
	defer s.oc.Mu.Unlock()
	if len(s.oc.Reloaded) != 2 || s.oc.Reloaded[0] != "/" || s.oc.Reloaded[1] != "/" {
		t.Fatalf("healthy location and retry: %v", s.oc.Reloaded)
	}
}

func TestCatalogReloadE2E(t *testing.T) {
	s := newStack(t, "127.84")
	ctx := context.Background()
	want(t, s.do(t, call{url: "/tools", as: &alice}), http.StatusOK)
	u, _ := s.st.UserByUID(ctx, alice.uid)
	h := notFromUserNets(catalogReload(s.cat), config.Config{UserNetPrefix: "10.90"})
	post := func(from string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/admin/catalog/reload", nil)
		r.RemoteAddr = from
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	busy := func(on bool) {
		s.oc.Mu.Lock()
		s.oc.Active = nil
		if on {
			s.oc.Active = []string{"ses_1"}
		}
		s.oc.Mu.Unlock()
	}
	// asleep: nothing to reload, the start hook applies the catalog
	want(t, post("192.0.2.1:1"), http.StatusOK, `{"pending":0,"reloaded":0}`)
	if _, err := s.svc.EnsureRunning(ctx, u.UID); err != nil {
		t.Fatal(err)
	}
	reloads := s.oc.Reloads
	want(t, post("192.0.2.1:1"), http.StatusOK, `{"pending":0,"reloaded":1}`)
	if s.oc.Reloads != reloads+1 {
		t.Fatalf("idle: reloads %d", s.oc.Reloads-reloads)
	}
	// busy: marked, reloaded by the first reap tick after the run ends, once
	busy(true)
	want(t, post("192.0.2.1:1"), http.StatusOK, `{"pending":1,"reloaded":0}`)
	s.svc.Reap(ctx)
	if s.oc.Reloads != reloads+1 {
		t.Fatal("reloaded while busy")
	}
	busy(false)
	s.svc.Reap(ctx)
	s.svc.Reap(ctx)
	if s.oc.Reloads != reloads+2 {
		t.Fatalf("after idle: reloads %d", s.oc.Reloads-reloads)
	}
	// the admin listener is reachable from the user networks (llm-gw): refused
	want(t, post("10.90.1.10:5000"), http.StatusForbidden)
	if s.oc.Reloads != reloads+2 {
		t.Fatal("reload from a user network")
	}
}
