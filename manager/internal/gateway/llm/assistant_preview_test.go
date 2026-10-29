// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
)

func TestDraftCompletionSettingsAndBudget(t *testing.T) {
	d, err := testinstance.Load("../../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Select(d)()
	p := d.Assistants[0]
	temp := 0.2
	top := 0.8
	tokens := assistants.Integer(100)
	p.Temperature = &temp
	p.TopP = &top
	p.MaxOutputTokens = &tokens
	p.ReasoningEffort = "low"
	p.ModelRole = "small"
	up, _ := url.Parse("http://model.test")
	record := &rec{}
	g := New(Gateway{Upstream: up, Usage: record, Log: slog.New(slog.DiscardHandler)})
	calls := 0
	g.proxy.Transport = assistantTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != d.Models.Small || body["temperature"] != temp || body["top_p"] != top || body["max_tokens"] != float64(tokens) || body["reasoning_effort"] != "low" || body["tools"] != nil {
			t.Fatal(body)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"answer"}}],"usage":{"prompt_tokens":50,"completion_tokens":20}}`)
	})}
	u := store.User{UID: "draft-owner", Username: "editor"}
	if _, err := g.AssistantDraftCompletion(context.Background(), u, "ses_profile_preview_test", p, []any{}); err != nil {
		t.Fatal(err)
	}
	if len(record.ev) != 1 || record.ev[0].UID != u.UID {
		t.Fatal("unmetered test")
	}
	g.Budget = &Budget{Spent: func(context.Context, string, time.Time) (float64, error) { return 5, nil }, CapEUR: 5, Now: time.Now}
	if _, err := g.AssistantDraftCompletion(context.Background(), u, "ses_profile_preview_test", p, []any{}); !errors.Is(err, ErrDayCap) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("budget refused after dispatch")
	}
}
