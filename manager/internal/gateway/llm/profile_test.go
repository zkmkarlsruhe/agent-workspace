// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type assistantTransport struct{ http.Handler }

func (t assistantTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.ServeHTTP(w, r)
	resp := w.Result()
	resp.Request = r
	return resp, nil
}

func TestAssistantCompletionMeteringAndRawContextDenial(t *testing.T) {
	up, _ := url.Parse("http://model.test")
	record := &rec{}
	g := New(Gateway{Upstream: up, APIKey: "manager-key", Users: users{}, Usage: record, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Log: slog.New(slog.DiscardHandler)})
	called := 0
	g.proxy.Transport = assistantTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Header.Get("Authorization") != "Bearer manager-key" || r.Header.Get("x-litellm-end-user-id") != "alice" || r.Header.Get("x-litellm-session-id") != "ses_profile_test" {
			t.Fatal("missing internal identity")
		}
		var body struct {
			Model string
			Tools []any
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "approved-model" || len(body.Tools) != 1 {
			t.Fatal(body)
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"wiki","arguments":"{}"}}]}}],"usage":{"prompt_tokens":50,"completion_tokens":20}}`)
	})}
	u := store.User{UID: "u1", Username: "alice"}
	raw, err := g.AssistantCompletion(context.Background(), u, "ses_profile_test", "approved-model", 4096, []any{}, []any{map[string]any{"type": "function"}})
	if err != nil || !strings.Contains(string(raw), "tool_calls") || len(record.ev) != 1 || record.ev[0].SessionID != "ses_profile_test" || record.ev[0].CostEUR == nil {
		t.Fatal(string(raw), err, record.ev)
	}
	g.Budget = &Budget{Spent: func(context.Context, string, time.Time) (float64, error) { return 5, nil }, CapEUR: 5, Now: time.Now}
	if _, err := g.AssistantCompletion(context.Background(), u, "ses_profile_test", "approved-model", 4096, nil, nil); !errors.Is(err, ErrDayCap) {
		t.Fatal(err)
	}
	for _, header := range []string{"x-opencode-session-id", "x-opencode-session"} {
		w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:1234", `{"messages":[]}`, map[string]string{header: "ses_profile_test"})
		if w.Code != 403 {
			t.Fatalf("raw context accepted %d", w.Code)
		}
	}
	if called != 1 {
		t.Fatalf("refused call reached model %d", called)
	}
}
