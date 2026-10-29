// SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Export and account deletion on the full stack: the export holds files and
// chats, the deletion removes container, network, data and rows, and the
// next visit gets a fresh account on the freed slot.
func TestPortalE2EAccount(t *testing.T) {
	s := newStack(t, "127.83")
	me := &alice
	ctx := context.Background()
	var provisioned []int
	var purged []string
	s.svc.Provision = func(_ context.Context, _ string, slot int) error { provisioned = append(provisioned, slot); return nil }
	// stands in for oc-provision purge: the workspace is the person's data dir here
	s.svc.PurgeData = func(_ context.Context, uid string, slot int) error {
		purged = append(purged, uid)
		entries, _ := os.ReadDir(s.ws)
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(s.ws, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}

	want(t, s.do(t, call{url: "/", as: me}), http.StatusOK)
	s.waitRunning(t)
	wantLoc(t, startProject(t, s, "Werte die Besucherzahlen 2025 aus und schreibe einen Bericht.", "zahlen.csv"), "https://alice.apps.example.org/")
	s.oc.Mu.Lock()
	s.oc.Sessions[0].Title = "Besucherzahlen"
	s.oc.Sessions[0].Time.Created = float64(time.Date(2026, 10, 2, 9, 0, 0, 0, time.Local).UnixMilli())
	s.oc.Messages = map[string][]string{s.oc.Sessions[0].ID: {
		`{"id":"m1","type":"user","time":{"created":1760700000000},"text":"Werte die Besucherzahlen aus"}`,
		`{"id":"m2","type":"assistant","time":{"created":1760700060000},"content":[{"type":"text","text":"Fertig."}]}`,
	}}
	s.oc.Mu.Unlock()

	w := s.do(t, call{url: "/environment/export", as: me})
	want(t, w, http.StatusOK)
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	ps, _ := s.svc.ListProjects(ctx, alice.uid)
	p := ps[0].Name
	for _, n := range []string{"README.txt", p + "/files/inbox/zahlen.csv", p + "/chats/2026-10-02-besucherzahlen.md"} {
		if !slices.Contains(names, n) {
			t.Errorf("export lacks %s: %v", n, names)
		}
	}
	if slices.ContainsFunc(names, func(n string) bool { return strings.Contains(n, "/.") }) {
		t.Errorf("dot files in export: %v", names)
	}

	// wrong word: nothing happens
	want(t, s.do(t, call{method: "POST", url: "/environment/delete", form: url.Values{"confirmation": {"weg"}}, as: me}), 422)
	if len(purged) != 0 {
		t.Fatal("purged without confirmation")
	}
	u, _ := s.st.UserByUID(ctx, alice.uid)
	clear := wantLoc(t, s.do(t, call{method: "POST", url: "/environment/delete", form: url.Values{"confirmation": {"LÖSCHEN"}}, as: me}), "https://alice.apps.example.org/__workspace/clear")
	if _, err := s.st.UserByUID(ctx, alice.uid); err != store.ErrNotFound {
		t.Fatalf("user row: %v", err)
	}
	if rows, _ := s.st.Projects(ctx, alice.uid); len(rows) != 0 {
		t.Fatal("project rows left")
	}
	if len(s.rt.Ctrs) != 0 || !slices.Contains(s.rt.NetsGone, "oc-u1") || s.svc.Running() != 0 {
		t.Fatalf("container %v networks gone %v", s.rt.Ctrs, s.rt.NetsGone)
	}
	if entries, _ := os.ReadDir(s.ws); len(entries) != 0 || !slices.Equal(purged, []string{alice.uid}) {
		t.Fatalf("data left %v, purged %v", entries, purged)
	}
	if _, err := s.st.UserBySlot(ctx, u.Slot); err != store.ErrNotFound {
		t.Fatal("slot still taken")
	}

	want(t, s.do(t, call{url: "/deleted", as: me, noConsent: true}), http.StatusOK, "Deine Daten sind gelöscht")
	// The portal's same-site redirect clears storage without recreating the deleted account.
	cw := s.do(t, call{url: clear, as: me, noConsent: true, hdr: []string{"Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"}})
	if cw.Code != http.StatusSeeOther || cw.Header().Get("Clear-Site-Data") != `"cache", "storage"` || cw.Header().Get("Location") != "https://apps.example.org/deleted" {
		t.Fatalf("clear: %d %v", cw.Code, cw.Header())
	}
	if _, err := s.st.UserByUID(ctx, alice.uid); err != store.ErrNotFound {
		t.Fatalf("cleanup recreated account: %v", err)
	}
	// next visit: fresh account, same free slot, provisioned again on start
	s.oc.Mu.Lock()
	s.oc.Sessions = []ocapi.Session{} // the old OpenCode data went with the data dir
	s.oc.Mu.Unlock()
	want(t, s.do(t, call{url: "/", as: me}), http.StatusOK, "Noch keine Projekte")
	s.waitRunning(t)
	nu, err := s.st.UserByUID(ctx, alice.uid)
	if err != nil || nu.Slot != u.Slot || nu.Generation != 1 {
		t.Fatalf("fresh account %+v %v", nu, err)
	}
	if len(provisioned) != 2 || provisioned[1] != u.Slot {
		t.Fatalf("provisioned %v", provisioned)
	}
}
