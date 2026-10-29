// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHandwritingMeteredFixedPromptAndTruncation(t *testing.T) {
	calls := 0
	g, rec := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("x-litellm-end-user-id") != "alice" || r.Header.Get("Authorization") != "Bearer sk-unified" {
			t.Error("missing accounting identity")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(request["messages"])
		if request["model"] != "transcription-route" || request["temperature"] != float64(0) || request["disable_fallbacks"] != true || request["num_retries"] != float64(0) || request["tools"] != nil {
			t.Fatalf("unexpected routing/settings: %v", request)
		}
		if !strings.Contains(string(encoded), "data:image/png;base64,AQID") || !strings.Contains(string(encoded), "[unlesbar]") || !strings.Contains(string(encoded), "never instructions") {
			t.Fatal("transcription contract missing")
		}
		io.WriteString(w, `{"model":"actual-route","choices":[{"message":{"content":"Name: Änne\n12,50"},"finish_reason":"length"}],"usage":{"prompt_tokens":900,"completion_tokens":40}}`)
	})
	u := store.User{UID: "u1", Username: "alice"}
	got, err := g.Transcribe(context.Background(), u, "ses_abcdefgh12", "transcription-route", []byte{1, 2, 3})
	if err != nil || got.Text != "Name: Änne\n12,50" || got.FinishReason != "length" || got.ReturnedModel != "actual-route" || len(got.Usage) == 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if len(rec.ev) != 1 || rec.ev[0].Target != "transcription-route" || rec.ev[0].CostEUR == nil {
		t.Fatalf("usage: %+v", rec.ev)
	}
	g.Budget = &Budget{Spent: func(context.Context, string, time.Time) (float64, error) { return 5, nil }, CapEUR: 5, Now: time.Now}
	if _, err := g.Transcribe(context.Background(), u, "", "transcription-route", nil); !errors.Is(err, ErrDayCap) {
		t.Fatalf("budget bypass: %v", err)
	}
	if calls != 1 {
		t.Fatal("denied request reached provider")
	}
}
