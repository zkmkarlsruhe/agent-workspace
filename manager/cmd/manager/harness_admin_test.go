// SPDX-License-Identifier: MIT

//go:build portalharness

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// go test -tags portalharness -run TestHarnessAdmin -timeout 0 ./cmd/manager
// Browse http://127.0.0.1:47826/admin as alice (member of operations-admins), with
// the demo data of TestHarness collected from the fake OpenCode, a second
// person, and an evaluation by a fake model.
func TestHarnessAdmin(t *testing.T) {
	s := newStack(t, "127.0")
	seed(t, s)
	ctx := context.Background()
	if err := s.st.AcceptConsent(ctx, alice.uid, consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	s.cfg.AdminGroup, s.svc.CollectChats = "operations-admins", true
	aw := newEvaluation(s.st, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	aw.Complete = func(_ context.Context, _, user string) (string, error) {
		switch {
		case strings.Contains(user, "Besucherzahlen"):
			return `{"topic":"Besucherzahlen 2025 nach Monat","outcome":"Tabelle und Diagramm erstellt","success":"yes","problems":[],
				"private_data":false,"misuse":false,"improvements":[{"kind":"missing-skill","text":"Diagramm-Vorlage im Workspace-Stil"}]}`, nil
		case strings.Contains(user, "Inventar"):
			return `{"topic":"Inventar mit Fotos abgleichen","outcome":"Abgleich abgebrochen","success":"no","problems":["misunderstanding"],
				"private_data":false,"misuse":false,"improvements":[{"kind":"missing-tool","text":"Bilder in Ordnern vergleichen"}]}`, nil
		case strings.Contains(user, "Bewerbung"):
			return `{"topic":"Bewerbungen sichten","outcome":"Liste der Bewerbungen","success":"partly","problems":[],
				"private_data":true,"private_data_reason":"Lebensläufe mit Adressen und Geburtsdaten eingefügt",
				"misuse":false,"improvements":[{"kind":"missing-guide","text":"Hinweis zu Personaldaten in der Hilfe"}]}`, nil
		}
		return `{"topic":"Pressetext Lichthof","outcome":"Pressetext mit 2000 Zeichen","success":"yes","problems":[],
			"private_data":false,"misuse":false,"improvements":[]}`, nil
	}
	portalAdmin = aw
	s.buildEdge(t)

	user := `{"id":"u%d","type":"user","time":{"created":1760700000000},"text":"%s"}`
	reply := `{"id":"a%d","type":"assistant","time":{"created":1760700060000},"content":[%s{"type":"text","text":"%s"}]}`
	s.oc.Mu.Lock()
	s.oc.Messages = map[string][]string{
		"ses_b1000000": {fmt.Sprintf(user, 1, "Werte inbox/besucher-2025.csv nach Monat aus"),
			fmt.Sprintf(reply, 2, `{"type":"tool","name":"bash","state":{"status":"completed","input":{"command":"python3 work/auswertung.py"}}},`, "Die Besucherzahlen steigen von 1200 im Januar auf 1380 im Februar.")},
		"ses_p1000000": {fmt.Sprintf(user, 3, "Schreibe einen Pressetext zur Ausstellung im Lichthof"),
			fmt.Sprintf(reply, 4, `{"type":"tool","name":"webfetch","state":{"status":"completed","input":{"url":"https://example.org/de/ausstellungen"}}},`, "Hier ist ein Entwurf mit etwa 2000 Zeichen.")},
		"ses_i1000000": {fmt.Sprintf(user, 5, "Gleiche das Inventar mit den Fotos ab"),
			fmt.Sprintf(reply, 6, `{"type":"tool","name":"bash","state":{"status":"error","input":{"command":"identify inbox/fotos/*.jpg"},"error":{"message":"identify: command not found"}}},`+
				`{"type":"tool","name":"read","state":{"status":"error","input":{"filePath":"inbox/fotos/inv-0001.jpg"},"error":{"message":"binary file"}}},`, "Ich kann die Fotos leider nicht öffnen.")},
	}
	active := s.oc.Active
	s.oc.Active = nil
	s.oc.Mu.Unlock()
	s.svc.Reap(ctx)
	s.oc.Mu.Lock()
	s.oc.Active = active
	s.oc.Mu.Unlock()

	bob := identity.Identity{UID: "c7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356", Username: "bob", Name: "Bob Example"}
	if _, err := s.st.EnsureUser(ctx, bob); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.st.PutChat(ctx, store.Chat{SessionID: "ses_o1000000", UID: bob.UID, Project: "personal", Title: "Bewerbungen sichten",
		Created: now.Add(-3 * time.Hour), Updated: now.Add(-2 * time.Hour), CollectedAt: now, Messages: 6, ToolCalls: 2,
		Transcript: "## Du, heute\n\nHier sind die Bewerbungen ...\n\n## Helfer, heute\n\nIch habe die Bewerbungen sortiert.\n\n- Werkzeug read: inbox/bewerbungen.pdf (fertig)\n"})
	if _, err := aw.Run(ctx); err != nil {
		t.Fatal(err)
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Authentik-Uid", alice.uid)
		r.Header.Set("X-Authentik-Username", alice.username)
		r.Header.Set("X-Authentik-Name", "the operator")
		r.Header.Set("X-Authentik-Groups", "opencode-users|operations-admins")
		r.Host = "apps.example.org"
		if r.Header.Get("Origin") != "" {
			r.Header.Set("Origin", "https://apps.example.org")
		}
		s.edge.ServeHTTP(w, r)
	})
	t.Log("portal on http://127.0.0.1:47826/admin")
	t.Fatal(http.ListenAndServe("127.0.0.1:47826", h))
}
