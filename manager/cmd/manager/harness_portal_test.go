// SPDX-License-Identifier: MIT

//go:build portalharness

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/co2"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// go test -tags portalharness -run TestHarness -timeout 0 ./cmd/manager
// Browse http://127.0.0.1:47820 as alice, with seeded demo data. Fake OpenCode
// listens on 127.0.1.10:4096; deep links point to alice.apps.example.org and do not resolve.
func TestHarness(t *testing.T) {
	s := newStack(t, "127.0")
	seed(t, s)
	seedTools(t, s)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Authentik-Uid", alice.uid)
		r.Header.Set("X-Authentik-Username", alice.username)
		r.Header.Set("X-Authentik-Name", "the operator")
		r.Header.Set("X-Authentik-Groups", alice.groups)
		r.Host = "apps.example.org"
		if r.Header.Get("Origin") != "" {
			r.Header.Set("Origin", "https://apps.example.org")
		}
		s.edge.ServeHTTP(w, r)
	})
	t.Log("portal on http://127.0.0.1:47820")
	t.Fatal(http.ListenAndServe("127.0.0.1:47820", h))
}

func seed(t *testing.T, s *stack) {
	ctx := context.Background()
	s.svc.DiskUsage = func(context.Context, int) (int64, int64, error) { return 7_700_000_000, 20 << 30, nil }
	if _, err := s.st.EnsureUser(ctx, identity.Identity{UID: alice.uid, Username: alice.username, Name: "the operator"}); err != nil {
		t.Fatal(err)
	}
	projects := []struct{ name, desc string }{
		{"besucherzahlen-2025", "Werte die Besucherzahlen 2025 aus und schreibe einen kurzen Bericht für die Direktion."},
		{"pressetext-lichthof", "Schreibe einen Pressetext zur neuen Ausstellung im Lichthof, etwa 2000 Zeichen."},
		{"inventar-medienkunst", "Gleiche die Inventarliste der Medienkunstwerke mit den Fotos im Eingangsordner ab."},
		{"alter-entwurf", "Erster Entwurf für das Vermittlungsprogramm."},
		{"plakat-herbst", "Schreibe drei kurze Textvarianten für das Herbstplakat."}, // fresh: no chats, no files
	}
	for _, p := range projects {
		if err := s.svc.CreateProject(ctx, alice.uid, lifecycle.NewProject{Name: p.name, Description: p.desc}); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"besucherzahlen-2025/inbox/besucher-2025.csv":        "monat;besucher\njan;1200\nfeb;1380\n",
		"besucherzahlen-2025/inbox/kassensystem-export.xlsx": "xlsx",
		"besucherzahlen-2025/results/bericht.md":             "# Besucherzahlen 2025\n",
		"besucherzahlen-2025/results/diagramme/monate.svg":   "<svg/>",
		"besucherzahlen-2025/work/auswertung.py":             "print(1)\n",
		"pressetext-lichthof/inbox/ausstellungstext.docx":    "docx",
		"pressetext-lichthof/results/pressetext.md":          "# Pressetext\n",
		"inventar-medienkunst/inbox/inventar.csv":            "nr;titel\n",
		"inventar-medienkunst/inbox/fotos/inv-0001.jpg":      "jpg",
		"inventar-medienkunst/inbox/fotos/inv-0002.jpg":      "jpg",
		"alter-entwurf/results/programm.md":                  "# Programm\n",
	}
	for p, c := range files {
		f := filepath.Join(s.ws, p)
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.svc.MoveToTrash(ctx, alice.uid, "alter-entwurf"); err != nil {
		t.Fatal(err)
	}
	// inactivity rule: one project goes to the trash in 5 days, one went there automatically
	for name, age := range map[string]time.Duration{"flyer-fruehjahr": 25 * 24 * time.Hour, "vermittlung-2024": 40 * 24 * time.Hour} {
		os.MkdirAll(filepath.Join(s.ws, name, "results"), 0o755)
		if err := s.st.CreateProject(ctx, store.Project{ID: "demo-" + name, UID: alice.uid, Folder: name, CreatedAt: time.Now().Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	s.svc.TrashIdle(ctx)
	// stands in for oc-provision: "Alle meine Daten löschen" works in the harness
	s.svc.PurgeData = func(context.Context, string, int) error {
		entries, _ := os.ReadDir(s.ws)
		for _, e := range entries {
			os.RemoveAll(filepath.Join(s.ws, e.Name()))
		}
		return nil
	}

	now := time.Now()
	chats := []struct {
		project, id, title, outcome string
		usd                         float64
		ago                         time.Duration
	}{
		{"besucherzahlen-2025", "ses_b1000000", "Besucherzahlen nach Monat", "succeeded", 0.84, 26 * time.Hour},
		{"besucherzahlen-2025", "ses_b2000000", "Diagramme für den Bericht", "", 0.31, 5 * time.Minute},
		{"pressetext-lichthof", "ses_p1000000", "Pressetext Lichthof", "succeeded", 0.12, 3 * time.Hour},
		{"inventar-medienkunst", "ses_i1000000", "Inventar mit Fotos abgleichen", "failed", 0.05, 50 * time.Hour},
	}
	s.oc.Mu.Lock()
	for _, c := range chats {
		se := ocapi.Session{ID: c.id, Title: c.title, Outcome: c.outcome, CostUSD: c.usd,
			Location: ocapi.Location{Directory: "/workspace/" + c.project}}
		se.Time.Created = float64(now.Add(-c.ago - time.Hour).UnixMilli())
		se.Time.Updated = float64(now.Add(-c.ago).UnixMilli())
		s.oc.Sessions = append(s.oc.Sessions, se)
	}
	s.oc.Active = []string{"ses_b2000000"}
	s.oc.Mu.Unlock()

	ids := map[string]string{}
	for _, p := range projects[:3] {
		sp, err := s.st.ProjectByFolder(ctx, alice.uid, p.name)
		if err != nil {
			t.Fatal(err)
		}
		ids[p.name] = sp.ID
	}
	trash, _ := s.svc.ListTrash(ctx, alice.uid)
	for _, tr := range trash {
		if t2, err := s.st.TrashItem(ctx, alice.uid, tr.ID); err == nil {
			ids["alter-entwurf"] = t2.Project.ID
		}
	}
	usage := []struct {
		project, session string
		eur              float64
		ago              time.Duration
	}{
		{"besucherzahlen-2025", "ses_b1000000", 0.52, 26 * time.Hour},
		{"besucherzahlen-2025", "ses_b1000000", 0.24, 25 * time.Hour},
		{"besucherzahlen-2025", "ses_b2000000", 0.28, 5 * time.Minute},
		{"pressetext-lichthof", "ses_p1000000", 0.11, 3 * time.Hour},
		{"inventar-medienkunst", "ses_i1000000", 0.05, 50 * time.Hour},
		{"alter-entwurf", "ses_a1000000", 0.37, 9 * 24 * time.Hour},
		{"", "", 0.02, 2 * time.Hour}, // name suggestion, no project
	}
	for i, u := range usage {
		at := now.Add(-u.ago)
		// tokens scaled from the docs/adr/README.md B.2.4 example (0,83 EUR = 1.2 M in, 80 % cached, 30 k out)
		in, out := int64(u.eur/0.83*1_200_000), int64(u.eur/0.83*30_000)
		whLo, whHi, gLo, gHi, _ := co2.Estimate("default", in, in*8/10, out)
		if err := s.st.InsertUsage(ctx, store.UsageEvent{EventID: "demo-" + string(rune('a'+i)), Start: at, End: at.Add(20 * time.Second),
			UID: alice.uid, ProjectID: ids[u.project], SessionID: u.session, RootSessionID: u.session, Capability: "llm",
			Target: "default", Status: 200, CostEUR: &u.eur, InTokens: &in, OutTokens: &out,
			WhLo: &whLo, WhHi: &whHi, CO2LoG: &gLo, CO2HiG: &gHi, MethodVersion: co2.Version}); err != nil {
			t.Fatal(err)
		}
	}
}
