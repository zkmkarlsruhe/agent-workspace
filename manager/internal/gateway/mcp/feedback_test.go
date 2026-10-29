// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// feedback_send: the manager attaches the excerpt itself (session id from
// _meta, never from the arguments), validates, limits per person, answers
// in German.
func TestFeedbackSend(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var asked []string
	ctxErr := error(nil)
	lang := "de"
	s := New(Server{SearXNG: mustURL("http://127.0.0.1:1"), SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf,
		Users: users{}, Usage: &rec{}, Log: slog.New(slog.DiscardHandler), Feedback: st,
		Lang: func(context.Context, string) (string, error) { return lang, nil },
		FeedbackContext: func(_ context.Context, uid, session string, n int) (lifecycle.FeedbackContext, error) {
			asked = append(asked, fmt.Sprintf("%s %s %d", uid, session, n))
			if ctxErr != nil {
				return lifecycle.FeedbackContext{Version: "2.2.0"}, ctxErr
			}
			return lifecycle.FeedbackContext{Project: "bericht", SessionID: session, Version: "2.2.0",
				Messages: "\n## Du\n\nes klemmt\n\n## Helfer\n\n- Werkzeug bash: ls (Fehler: kaputt)\n"}, nil
		}})
	const me = "10.90.1.10:40000"
	if l := rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String(); !strings.Contains(l, `"feedback_send"`) ||
		!strings.Contains(l, "das liest nur dieses Team") {
		t.Fatalf("list: %s", l)
	}
	call := func(args string) toolResult {
		t.Helper()
		w := rpc(s, me, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"feedback_send","arguments":`+args+
			`,"_meta":{"ai.opencode/sessionID":"ses_0123456789ab"}}}`)
		var r struct {
			Result toolResult `json:"result"`
		}
		json.Unmarshal(w.Body.Bytes(), &r)
		if len(r.Result.Content) == 0 {
			t.Fatalf("no result: %s", w.Body)
		}
		return r.Result
	}
	for _, bad := range []string{`{}`, `{"message":"  "}`, `{"message":"x","kind":"wut"}`,
		`{"message":"` + strings.Repeat("ä", FeedbackMaxText+1) + `"}`} {
		if r := call(bad); !r.IsError {
			t.Fatalf("%.40s accepted: %+v", bad, r)
		}
	}
	r := call(`{"message":"Die PDF-Erzeugung klemmt","context_messages":99}`)
	if r.IsError || !strings.Contains(r.Content[0].Text, "letzten 2 Nachrichten") || !strings.Contains(r.Content[0].Text, "nur dieses Team") {
		t.Fatalf("%+v", r)
	}
	if len(asked) != 1 || asked[0] != "u1 ses_0123456789ab 20" {
		t.Fatalf("context asked %q", asked)
	}
	l, _ := st.FeedbackList(context.Background(), 5)
	if len(l) != 1 || l[0].Kind != "problem" || l[0].Source != "chat" || l[0].Project != "bericht" || l[0].Version != "2.2.0" {
		t.Fatalf("%+v", l)
	}
	if f, _ := st.FeedbackByID(context.Background(), l[0].ID); !strings.Contains(f.Context, "Werkzeug bash") {
		t.Fatalf("%+v", f)
	}

	// the excerpt fails: the words still arrive, the answer says so
	ctxErr = errors.New("container weg")
	if r := call(`{"message":"Lob","kind":"praise"}`); r.IsError || !strings.Contains(r.Content[0].Text, "ohne Chat-Nachrichten") {
		t.Fatalf("without excerpt: %+v", r)
	}
	ctxErr = nil
	lang = "en"
	if r := call(`{"message":"Frage","kind":"question"}`); r.IsError || !strings.Contains(r.Content[0].Text, "only that team reads them") {
		t.Fatalf("en: %+v", r)
	}
	if l := rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String(); !strings.Contains(l, "Feedback to the team") ||
		!strings.Contains(l, "only that team reads them") || !strings.Contains(l, "how many of the last chat messages") {
		t.Fatalf("en list: %s", l)
	}
	lang = "de"
	for i := 3; i < FeedbackPerDay; i++ {
		if r := call(`{"message":"noch eins","kind":"idea"}`); r.IsError {
			t.Fatalf("%d: %+v", i, r)
		}
	}
	if r := call(`{"message":"zu viel"}`); !r.IsError || !strings.Contains(r.Content[0].Text, "24 Stunden") {
		t.Fatalf("limit: %+v", r)
	}
}
