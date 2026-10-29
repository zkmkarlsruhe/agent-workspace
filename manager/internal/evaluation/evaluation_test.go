// SPDX-License-Identifier: MIT

package evaluation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

const (
	uidA = "a7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356"
	uidB = "b7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356"
)

func TestParse(t *testing.T) {
	e, err := Parse("Hier:\n```json\n" + `{"topic":"  Pressetext\nLichthof ","outcome":"Text fertig","success":"Partly",
		"problems":["tool-error","quatsch","tool-error"],"private_data":true,"private_data_reason":"Gehaltsliste",
		"misuse":false,"improvements":[{"kind":"missing-skill","text":"Pressetext-Vorlage"},{"kind":"neu","text":"x"},{"kind":"other","text":""}]}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if e.Topic != "Pressetext Lichthof" || e.Success != "partly" || !slices.Equal(e.Problems, []string{"tool-error"}) ||
		!e.PrivateData || e.PrivateDataReason != "Gehaltsliste" ||
		!slices.Equal(e.Improvements, []Idea{{"missing-skill", "Pressetext-Vorlage"}, {"other", "x"}}) {
		t.Fatalf("%+v", e)
	}
	for _, bad := range []string{"", "kein json", `{"topic":"x","success":"maybe"}`, `{"topic":"","success":"yes"}`, `{"topic":1}`} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if c := cut(strings.Repeat("ä", 100), 11); c != "äääää ..." {
		t.Fatal(c)
	}
}

type fixture struct {
	st  *store.Store
	s   *Service
	now time.Time
}

func setup(t *testing.T) fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	for _, id := range []identity.Identity{{UID: uidA, Username: "alice", Name: "the operator"}, {UID: uidB, Username: "bob"}} {
		if _, err := st.EnsureUser(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := st.AcceptConsent(ctx, id.UID, consent.Version(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	f := fixture{st: st, now: now}
	f.s = &Service{Store: st, Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }, DayStart: config.DayStart}
	chats := []store.Chat{
		{SessionID: "ses_aaaaaaaa", UID: uidA, Project: "presse", Title: "Pressetext", Transcript: "## Du\n\nPressetext bitte\n", Messages: 2},
		{SessionID: "ses_bbbbbbbb", UID: uidA, Project: "zahlen", Title: "Besucherzahlen", Transcript: "MODELL-KAPUTT", ToolCalls: 3, ToolErrors: 2},
		{SessionID: "ses_cccccccc", UID: uidB, Project: "privat", Title: "Gehälter", Transcript: "GEHALT", Aborted: 1},
	}
	for _, c := range chats {
		c.Created, c.Updated, c.CollectedAt = now.Add(-2*time.Hour), now.Add(-time.Hour), now
		if err := st.PutChat(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	eur := 0.5
	for i, u := range []string{uidA, uidB, store.ManagerUID} {
		ev := store.UsageEvent{EventID: "e" + u[:3], Start: now.Add(-time.Hour), End: now, UID: u, Capability: "llm", Status: 200, CostEUR: &eur}
		if u != store.ManagerUID {
			ev.SessionID, ev.RootSessionID = chats[i].SessionID, chats[i].SessionID
		}
		if err := st.InsertUsage(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// fakeModel answers per transcript marker: valid JSON, garbage, or an error.
func fakeModel(calls *int) func(context.Context, string, string) (string, error) {
	return func(_ context.Context, system, user string) (string, error) {
		*calls++
		switch {
		case strings.Contains(user, "MODELL-KAPUTT"):
			return "Das kann ich nicht beurteilen.", nil
		case strings.Contains(user, "GEHALT"):
			return `{"topic":"Gehaltsliste sortieren","outcome":"Liste sortiert","success":"no","problems":["misunderstanding"],
				"private_data":true,"private_data_reason":"Gehälter mit Namen eingefügt","misuse":false,
				"improvements":[{"kind":"missing-guide","text":"Hinweis zu Personaldaten"}]}`, nil
		case strings.Contains(user, "Pressetext"):
			return `{"topic":"Pressetext Lichthof","outcome":"Text","success":"yes","problems":[],"private_data":false,
				"misuse":false,"improvements":[{"kind":"missing-skill","text":"Pressetext-Vorlage"}]}`, nil
		}
		return "", errors.New("down")
	}
}

func evalOf(t *testing.T, st *store.Store, id string) (Item, string) {
	t.Helper()
	c, err := st.ChatByID(context.Background(), id)
	if err != nil || c.Eval == nil {
		t.Fatalf("%s: %v %v", id, err, c.Eval)
	}
	return item(c), c.Eval.Source
}

func TestRunAndReport(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	calls := 0
	f.s.Complete = fakeModel(&calls)
	res, err := f.s.Run(ctx)
	if err != nil || res.LLM != 2 || res.Heuristic != 1 || calls != 3 {
		t.Fatalf("%+v %v calls %d", res, err, calls)
	}
	a, src := evalOf(t, f.st, "ses_aaaaaaaa")
	if src != "llm" || a.E.Topic != "Pressetext Lichthof" || len(a.Flags) != 0 {
		t.Fatalf("%s %+v %v", src, a.E, a.Flags)
	}
	b, src := evalOf(t, f.st, "ses_bbbbbbbb")
	if src != "heuristic" || b.E.Topic != "Besucherzahlen" || !slices.Equal(b.E.Problems, []string{"tool-error"}) ||
		!slices.Equal(b.Flags, []string{"tool-error"}) {
		t.Fatalf("fallback: %s %+v %v", src, b.E, b.Flags)
	}
	c, _ := evalOf(t, f.st, "ses_cccccccc")
	// measured abort joins the model's problems
	if !slices.Equal(c.E.Problems, []string{"misunderstanding", "aborted"}) ||
		!slices.Equal(c.Flags, []string{"aborted", "unsuccessful", "private-data"}) {
		t.Fatalf("%+v %v", c.E, c.Flags)
	}

	// incremental: nothing changed, nothing asked; a changed chat is evaluated again
	if res, _ := f.s.Run(ctx); res.LLM+res.Heuristic != 0 || calls != 3 {
		t.Fatalf("second run: %+v", res)
	}
	ch, _ := f.st.ChatByID(ctx, "ses_aaaaaaaa")
	ch.Updated = ch.Updated.Add(time.Minute)
	f.st.PutChat(ctx, ch.Chat)
	if res, _ := f.s.Run(ctx); res.LLM != 1 || calls != 4 {
		t.Fatalf("changed chat: %+v", res)
	}

	md, err := f.s.Report(ctx, f.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Auswertung Agent Workspace, 17.10.2026", "- Aktive Personen: 2\n", "- Kosten: 1,00 € (Auswertung selbst: 0,50 €)",
		"- Werkzeugfehler: 2 in 1 Chats", "- Erfolgreich (KI-Urteil): ja 1, teilweise 0, nein 1",
		"- Pressetext Lichthof (1)", "- tool-error: 1 Chats", "- missing-skill: Pressetext-Vorlage (1)",
		"- [Gehaltsliste sortieren](/admin/chats/ses_cccccccc): private Daten: Gehälter mit Namen eingefügt; nicht erfolgreich: Liste sortiert"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	for _, not := range []string{"alice", "bob", "Example"} {
		if strings.Contains(md, not) {
			t.Errorf("report names a person: %q", not)
		}
	}

	// without a model: heuristics only
	f.s.Complete = nil
	ch.Updated = ch.Updated.Add(time.Minute)
	f.st.PutChat(ctx, ch.Chat)
	if res, _ := f.s.Run(ctx); res.Heuristic != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestReportEscapesSessionID(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// Crafted ids never reach the store; the report escapes whatever it lists anyway.
	id := "ses_[crafted]\n#heading"
	if err := f.st.PutChat(ctx, store.Chat{SessionID: id, UID: uidA, Project: "presse", Title: "Flagged", Transcript: "GEHALT", Created: f.now, Updated: f.now, CollectedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ChatByID(ctx, id); err == nil {
		t.Fatal("crafted session id was stored")
	}
	md, err := f.s.Report(ctx, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(md, "crafted") {
		t.Fatalf("crafted session ID in report:\n%s", md)
	}
}

func TestChatAuditAndDeletion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.s.Run(ctx) // heuristics: Complete is nil
	it, err := f.s.Chat(ctx, Reader{UID: uidA, Username: "alice"}, "ses_cccccccc")
	if err != nil || it.Transcript != "GEHALT" || it.Username != "bob" {
		t.Fatalf("%+v %v", it, err)
	}
	au, _ := f.s.Audit(ctx, 10)
	if len(au) != 1 || au[0].AdminName != "alice" || au[0].SessionID != "ses_cccccccc" || au[0].Do != "chat_read" {
		t.Fatalf("%+v", au)
	}
	if _, err := f.s.Chat(ctx, Reader{UID: uidA, Username: "alice"}, "ses_x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}

	before, _ := f.st.DayStat(ctx, f.now.Add(-24*time.Hour), f.now)
	d, err := f.st.DeleteUser(ctx, uidB, "geloescht-0123456789abcdef")
	if err != nil || d.Chats != 1 {
		t.Fatal(d, err)
	}
	if _, err := f.st.ChatByID(ctx, "ses_cccccccc"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("deleted person's chat still visible")
	}
	after, _ := f.st.DayStat(ctx, f.now.Add(-24*time.Hour), f.now)
	if after != before {
		t.Fatalf("aggregates changed: %+v -> %+v", before, after)
	}
	list, _ := f.s.Chats(ctx, Filter{From: f.now.Add(-24 * time.Hour), To: f.now})
	if len(list.Items) != 2 {
		t.Fatalf("%d chats listed", len(list.Items))
	}
}

// Only people with an account and the current consent reach the model, checked
// for the run and again right before each call.
func TestEvaluationConsent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// ses_cccccccc (bob) is the newest change, so the run reaches it last
	ch, _ := f.st.ChatByID(ctx, "ses_cccccccc")
	ch.Updated = ch.Updated.Add(time.Minute)
	f.st.PutChat(ctx, ch.Chat)
	var seen []string
	f.s.Complete = func(_ context.Context, _, user string) (string, error) {
		seen = append(seen, user)
		// bob accepts an old notice only while the run is under way
		f.st.AcceptConsent(ctx, uidB, "old", time.Now())
		return "", errors.New("down")
	}
	if res, err := f.s.Run(ctx); err != nil || res.Heuristic != 2 || len(seen) != 2 {
		t.Fatalf("%+v %v %d calls", res, err, len(seen))
	}
	for _, u := range seen {
		if strings.Contains(u, "GEHALT") {
			t.Fatal("transcript sent after the consent changed")
		}
	}
	if c, _ := f.st.ChatByID(ctx, "ses_cccccccc"); c.Eval != nil {
		t.Fatal("chat without current consent evaluated")
	}
	// the next run does not even list it; a deleted account is out as well
	if _, err := f.st.DeleteUser(ctx, uidA, "deleted-00"); err != nil {
		t.Fatal(err)
	}
	if l, err := f.st.ChatsToEvaluate(ctx, consent.Version(), 10); err != nil || len(l) != 0 {
		t.Fatalf("%d listed, %v", len(l), err)
	}
	if ok, err := f.st.MayEvaluateChat(ctx, uidB, "ses_cccccccc", consent.Version()); ok || err != nil {
		t.Fatal(ok, err)
	}
	f.st.AcceptConsent(ctx, uidB, consent.Version(), time.Now())
	if l, _ := f.st.ChatsToEvaluate(ctx, consent.Version(), 10); len(l) != 1 || l[0].SessionID != "ses_cccccccc" {
		t.Fatalf("%+v", l)
	}
	// consent withdrawn while the model runs, after MayEvaluateChat: nothing stored
	seen = nil
	f.s.Complete = func(_ context.Context, _, user string) (string, error) {
		seen = append(seen, user)
		f.st.AcceptConsent(ctx, uidB, "old", time.Now())
		return "", errors.New("down")
	}
	if res, err := f.s.Run(ctx); err != nil || res.Heuristic != 0 || res.LLM != 0 || len(seen) != 1 {
		t.Fatalf("%+v %v %d calls", res, err, len(seen))
	}
	if c, _ := f.st.ChatByID(ctx, "ses_cccccccc"); c.Eval != nil {
		t.Fatal("evaluation stored after the consent was withdrawn")
	}
}

func TestChatsAndReportTruncated(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := range 498 {
		c := store.Chat{UID: uidA, SessionID: fmt.Sprintf("ses_limit%08d", i), Created: f.now.Add(-time.Hour), Updated: f.now.Add(-time.Hour), CollectedAt: f.now}
		if err := f.st.PutChat(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	list, err := f.s.Chats(ctx, Filter{From: f.now.Add(-24 * time.Hour), To: f.now})
	if err != nil || len(list.Items) != 500 || !list.Truncated {
		t.Fatalf("rows %d, truncated %v: %v", len(list.Items), list.Truncated, err)
	}
	report, err := f.s.Report(ctx, f.now)
	if err != nil || !strings.Contains(report, german("Chat limit reached; this report includes only the newest 500 chats.\n\n")) {
		t.Fatal("report truncation notice missing", err)
	}
}
