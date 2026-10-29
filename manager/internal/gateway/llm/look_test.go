// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Look sends one image with the manager's key and the person as end user,
// metered on the vision model and held against the person's budget.
func TestLookMeteredAndBudgeted(t *testing.T) {
	var got struct {
		Model    string
		Messages []struct {
			Content []struct {
				Type     string
				Text     string
				ImageURL struct{ URL string } `json:"image_url"`
			}
		}
	}
	var session string
	g, rec := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-litellm-end-user-id") != "alice" || r.Header.Get("Authorization") != "Bearer sk-unified" {
			w.WriteHeader(401)
			return
		}
		session = r.Header.Get("x-litellm-session-id")
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"choices":[{"message":{"content":"Ein rotes Quadrat."}}],"usage":{"prompt_tokens":900,"completion_tokens":40}}`)
	})
	u := store.User{UID: "u1", Username: "alice"}
	out, err := g.Look(context.Background(), u, "ses_abcdefgh12", "workspace/vision", "Was ist zu sehen?", []byte{0xff, 0xd8}, 100)
	if err != nil || out != "Ein rotes Quadrat." {
		t.Fatal(out, err)
	}
	c := got.Messages[0].Content
	if got.Model != "workspace/vision" || len(c) != 2 || c[0].Text != "Was ist zu sehen?" || c[1].ImageURL.URL != "data:image/jpeg;base64,/9g=" || session != "ses_abcdefgh12" {
		t.Fatalf("request %+v session %q", got, session)
	}
	if len(rec.ev) != 1 || rec.ev[0].Target != "workspace/vision" || rec.ev[0].SessionID != "ses_abcdefgh12" || rec.ev[0].CostEUR == nil {
		t.Fatalf("%+v", rec.ev)
	}
	g.Budget = &Budget{Spent: func(context.Context, string, time.Time) (float64, error) { return 5, nil }, CapEUR: 5, Now: time.Now}
	if _, err := g.Look(context.Background(), u, "", "workspace/vision", "?", nil, 100); !errors.Is(err, ErrDayCap) {
		t.Fatalf("over the cap: %v", err)
	}
	if len(rec.ev) != 1 {
		t.Fatalf("refused call recorded: %+v", rec.ev[1:])
	}
}
