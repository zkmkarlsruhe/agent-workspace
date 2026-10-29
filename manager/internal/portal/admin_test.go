// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/evaluation"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type dateAdmin struct {
	fakeAdmin
	day    time.Time
	filter evaluation.Filter
}

func (a *dateAdmin) Report(_ context.Context, day time.Time) (string, error) {
	a.day = day
	return "report for " + day.Format("2006-01-02"), nil
}

func (a *dateAdmin) Chats(_ context.Context, f evaluation.Filter) (evaluation.ChatList, error) {
	a.filter = f
	return evaluation.ChatList{}, nil
}

func TestAdminDateFilters(t *testing.T) {
	a := &dateAdmin{}
	h, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup,
		Admin: a, AdminGroup: "authentik Admins", DayStart: config.DayStart})
	if err != nil {
		t.Fatal(err)
	}
	const day = "2025-03-30"
	for _, path := range []string{"/admin/report?day=" + day, "/admin?day=" + day} {
		w := do(t, h, req{path: path})
		wantStatus(t, w, http.StatusOK)
		wantBody(t, w, "report for "+day)
		if a.day.Format("2006-01-02 15:04") != day+" 00:00" {
			t.Fatalf("report day: %v", a.day)
		}
		if strings.HasPrefix(path, "/admin/report") && w.Header().Get("Content-Disposition") != `attachment; filename="evaluation-`+day+`.md"` {
			t.Fatal(w.Header())
		}
	}
	w := do(t, h, req{path: "/admin/chats?from=2025-03-28&to=" + day})
	wantStatus(t, w, http.StatusOK)
	if a.filter.From.Format("2006-01-02 15:04") != "2025-03-28 00:00" || a.filter.To.Format("2006-01-02 15:04") != "2025-03-31 00:00" {
		t.Fatalf("chat date range: %+v", a.filter)
	}
}

type truncatedAdmin struct{ fakeAdmin }

func (truncatedAdmin) Chats(context.Context, evaluation.Filter) (evaluation.ChatList, error) {
	return evaluation.ChatList{Truncated: true}, nil
}

func TestAdminTruncationNotices(t *testing.T) {
	ff := &fakeFeedback{sent: make([]store.Feedback, 200)}
	h, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup,
		Admin: truncatedAdmin{}, AdminGroup: "authentik Admins", Feedback: ff, DayStart: config.DayStart})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, text string }{
		{"/admin/chats?person=nobody", "Chat limit reached; filters and totals cover only the newest 500 chats."},
		{"/admin/feedback", "List limit reached; only the newest 200 feedback entries are shown."},
	} {
		for _, language := range []string{"en", "de"} {
			w := do(t, h, req{path: tc.path, header: map[string]string{"Accept-Language": language}})
			wantStatus(t, w, 200)
			wantBody(t, w, tr(language, tc.text))
		}
	}
	ff.sent = ff.sent[:199]
	w := do(t, h, req{path: "/admin/feedback", header: map[string]string{"Accept-Language": "en"}})
	if strings.Contains(w.Body.String(), "List limit reached") {
		t.Fatal("uncapped feedback marked truncated")
	}
}

func TestTranscriptLines(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        []transcriptLine
	}{
		{"empty", " \n ", []transcriptLine{{"", "text"}}},
		{"speaker", "## Du", []transcriptLine{{"Du", "head"}}},
		{"tool", "- Werkzeug web_read (fertig)", []transcriptLine{{"- Werkzeug web_read (fertig)", "tool"}}},
		{"tool error", "- Werkzeug web_read (Fehler: timeout)", []transcriptLine{{"- Werkzeug web_read (Fehler: timeout)", "error"}}},
		{"error", "Fehler: timeout", []transcriptLine{{"Fehler: timeout", "error"}}},
		{"truncated", "[... gekürzt]", []transcriptLine{{"[... gekürzt]", "truncated"}}},
		{"text", "text with Fehler: inside", []transcriptLine{{"text with Fehler: inside", "text"}}},
		{"multiple lines", "\n## Du\n\nhello\n", []transcriptLine{{"Du", "head"}, {"", "text"}, {"hello", "text"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transcriptLines(tc.input); !slices.Equal(got, tc.want) {
				t.Fatalf("transcriptLines(%q) = %#v, want %#v", tc.input, got, tc.want)
			}
		})
	}
}
