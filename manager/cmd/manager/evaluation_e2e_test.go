// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

var (
	admin   = person{alice.uid, "alice", "opencode-users|operations-admins"}
	visitor = person{"c7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356", "bob", "opencode-users"}
)

// /admin on the full edge: 404 outside the admin group, collection from the
// running environment, run button behind CSRF protection, transcript with
// highlighted tool errors, audit of every read, Markdown download.
func TestEvaluationE2E(t *testing.T) {
	s := newStack(t, "127.85")
	ctx := context.Background()
	s.cfg.AdminGroup, s.svc.CollectChats = "operations-admins", true
	aw := newEvaluation(s.st, nil, slog.New(slog.DiscardHandler))
	aw.Complete = func(context.Context, string, string) (string, error) {
		return `{"topic":"Webseite zusammenfassen","outcome":"Zusammenfassung","success":"partly","problems":[],
			"private_data":false,"misuse":false,"improvements":[{"kind":"missing-tool","text":"Shell ohne Fehler"}]}`, nil
	}
	portalAdmin = aw
	t.Cleanup(func() { portalAdmin = nil })
	s.buildEdge(t)

	want(t, s.do(t, call{url: "/", as: &admin}), http.StatusOK)
	if err := s.svc.CreateProject(ctx, admin.uid, lifecycle.NewProject{Name: "recherche"}); err != nil {
		t.Fatal(err)
	}
	se := ocapi.Session{ID: "ses_e2e00001", Title: "Workspace-Seite lesen", Location: ocapi.Location{Directory: "/workspace/recherche"}}
	se.Time.Created, se.Time.Updated = float64(time.Now().Add(-time.Hour).UnixMilli()), float64(time.Now().UnixMilli())
	s.oc.Mu.Lock()
	s.oc.Sessions = append(s.oc.Sessions, se)
	s.oc.Messages = map[string][]string{"ses_e2e00001": {
		`{"id":"m1","type":"user","time":{"created":1760700000000},"text":"Fasse example.org zusammen"}`,
		`{"id":"m2","type":"assistant","time":{"created":1760700060000},"content":[
		 {"type":"tool","id":"t1","name":"bash","state":{"status":"error","input":{"command":"curl example.org"},"error":{"message":"kein Netz"}}},
		 {"type":"text","text":"Das Workspace ist ein Museum."}]}`}}
	s.oc.Mu.Unlock()
	s.svc.Reap(ctx) // collects: the environment is running and idle

	// outside the admin group everything under /admin is the plain 404
	for _, c := range []call{{url: "/admin"}, {url: "/admin/chats"}, {url: "/admin/chats/ses_e2e00001"}, {url: "/admin/report"},
		{method: "POST", url: "/admin/evaluate"}} {
		c.as = &visitor
		w := s.do(t, c)
		want(t, w, http.StatusNotFound, "Seite nicht gefunden")
		if strings.Contains(w.Body.String(), "ses_e2e00001") || strings.Contains(w.Body.String(), `href="/admin"`) {
			t.Fatalf("%s leaks the admin area", c.url)
		}
	}
	if au, _ := s.st.Audit(ctx, 10); len(au) != 0 {
		t.Fatal("refused read was audited")
	}

	w := s.do(t, call{url: "/admin", as: &admin})
	want(t, w, http.StatusOK, "Nur zur Auswertung des Prototyps – Zugriff wird protokolliert", `href="/admin" aria-current="page">Auswertung`,
		"Letzte 14&nbsp;Tage", "Jetzt auswerten", "offen: 1")

	// the run button: cross-site refused, same origin starts a background run
	want(t, s.do(t, call{method: "POST", url: "/admin/evaluate", as: &admin, ctype: "application/x-www-form-urlencoded",
		body: strings.NewReader(""), hdr: []string{"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"}}), http.StatusForbidden)
	if running, last := aw.State(); running || !last.At.IsZero() {
		t.Fatal("cross-site POST started a run")
	}
	w = s.do(t, call{method: "POST", url: "/admin/evaluate", as: &admin, form: map[string][]string{}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin?notice=started" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Location"))
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if running, last := aw.State(); !running && !last.At.IsZero() {
			if last.LLM != 1 {
				t.Fatalf("%+v", last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
	}

	want(t, s.do(t, call{url: "/admin/chats", as: &admin}), http.StatusOK, "Workspace-Seite lesen", "Webseite zusammenfassen", "recherche",
		"Werkzeugfehler</span>", `href="/admin/chats/ses_e2e00001"`)
	if w := s.do(t, call{url: "/admin/chats?flag=private-data", as: &admin}); strings.Contains(w.Body.String(), "ses_e2e00001") {
		t.Fatal("flag filter ignored")
	}
	want(t, s.do(t, call{url: "/admin/chats?person=bob", as: &admin}), http.StatusOK, "Keine Chats in diesem Zeitraum.")

	w = s.do(t, call{url: "/admin/chats/ses_e2e00001", as: &admin})
	want(t, w, http.StatusOK, `<div class="transcript__error">- Werkzeug bash: curl example.org (Fehler: kein Netz)</div>`,
		"Das Workspace ist ein Museum.", "Webseite zusammenfassen", "tool-error", "missing-tool: Shell ohne Fehler", "Nur zur Auswertung des Prototyps")
	au, _ := s.st.Audit(ctx, 10)
	if len(au) != 1 || au[0].AdminUID != admin.uid || au[0].AdminName != "alice" || au[0].SessionID != "ses_e2e00001" {
		t.Fatalf("%+v", au)
	}
	want(t, s.do(t, call{url: "/admin/chats/ses_fehlt", as: &admin}), http.StatusNotFound)

	w = s.do(t, call{url: "/admin/report?day=" + time.Now().Format("2006-01-02"), as: &admin})
	want(t, w, http.StatusOK, "# Auswertung Agent Workspace", "Webseite zusammenfassen (1)", "missing-tool: Shell ohne Fehler (1)")
	if w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal(w.Header())
	}
	if strings.Contains(w.Body.String(), "alice") {
		t.Fatal("report names the person")
	}
}

// The evaluation's model calls are manager usage: in no person's totals or cap.
func TestEvaluationMetered(t *testing.T) {
	s := newStack(t, "127.86")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-litellm-end-user-id") != "agents-evaluation" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("x-litellm-response-cost", "0.002")
		io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}],"usage":{"prompt_tokens":3000,"completion_tokens":200}}`)
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	accounting := instance.DefaultAccounting()
	accounting.UpstreamCostCurrency = "EUR"
	gw := llm.New(llm.Gateway{Accounting: &accounting, Upstream: u, APIKey: "k", Users: s.st, Usage: s.st, Log: slog.New(slog.DiscardHandler)})
	aw := newEvaluation(s.st, gw, slog.New(slog.DiscardHandler))
	if _, err := aw.Complete(context.Background(), "sys", "chat"); err != nil {
		t.Fatal(err)
	}
	day := config.DayStart(time.Now())
	if eur, _ := s.st.SpentSince(context.Background(), store.ManagerUID, day); eur != 0.002 {
		t.Fatalf("manager usage %v", eur)
	}
	if d, _ := s.st.DayStat(context.Background(), day, day.Add(24*time.Hour)); d.People != 0 || d.CostEUR != 0 || d.EvalCostEUR != 0.002 {
		t.Fatalf("%+v", d)
	}
}
