// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

// The feedback excerpt: the last n rendered messages of the calling session,
// tool calls one line each, a running session if the id is missing, bounded.
func TestFeedbackContext(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.s.FeedbackContext(ctx, uid1, "ses_1", 6); err != ErrNotRunning {
		t.Fatal("stopped environment:", err)
	}
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	se := ocapi.Session{ID: "ses_1", Location: ocapi.Location{Directory: e.s.projectDir("bericht")}}
	e.oc.Sessions = append(e.oc.Sessions, se)
	old := `{"id":"m0","type":"user","text":"ALTE FRAGE"}`
	idle := `{"id":"m9","type":"idle"}`
	e.oc.Messages = map[string][]string{"ses_1": {old, tUser, tAssistant, idle}}

	fc, err := e.s.FeedbackContext(ctx, uid1, "ses_1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Project != "bericht" || fc.SessionID != "ses_1" || fc.Version != "2.2.0" || strings.Contains(fc.Messages, "ALTE FRAGE") ||
		!strings.Contains(fc.Messages, "Fasse https://example.com") || !strings.Contains(fc.Messages, "- Werkzeug bash: ls -la (Fehler: kaputt)") ||
		strings.Contains(fc.Messages, "LANGER SEITENTEXT") || strings.Index(fc.Messages, "## Du") > strings.Index(fc.Messages, "## Helfer") {
		t.Fatalf("%+v", fc)
	}

	e.oc.Active = []string{"ses_1"}
	if fc, err = e.s.FeedbackContext(ctx, uid1, "", 1); err != nil || fc.SessionID != "ses_1" {
		t.Fatalf("running session: %+v %v", fc, err)
	}

	big := `{"id":"mb","type":"user","text":"` + strings.Repeat("x", 9<<10) + `"}`
	e.oc.Messages["ses_1"] = []string{}
	for range 20 {
		e.oc.Messages["ses_1"] = append(e.oc.Messages["ses_1"], big)
	}
	if fc, _ = e.s.FeedbackContext(ctx, uid1, "ses_1", 20); len(fc.Messages) > FeedbackMax+200 ||
		!strings.HasPrefix(fc.Messages, "[... ältere Nachrichten gekürzt") || !strings.Contains(fc.Messages, "[... Nachricht gekürzt") {
		t.Fatalf("not bounded: %d bytes", len(fc.Messages))
	}
}
