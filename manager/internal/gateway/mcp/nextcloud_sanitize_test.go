// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ncsync"
)

type dirtySync struct{ fakeSync }

func (*dirtySync) Search(context.Context, string, string, string, int) ([]ncsync.Entry, error) {
	return []ncsync.Entry{{Path: "Team/re\u200bport\nname.txt"}, {Path: "Team/fol\u202eder\nname", Dir: true}}, nil
}

type handlerTransport struct{ http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	resp := w.Result()
	resp.Request = r
	return resp, nil
}

func TestNextcloudListingSanitized(t *testing.T) {
	s := &Server{Log: slog.New(slog.DiscardHandler), Nextcloud: &Nextcloud{
		Base: mustURL("https://cloud.example"), Sync: &dirtySync{},
		Creds: func(context.Context, string) (string, string, string, error) { return "m s", "m s", "pw", nil },
	}}
	s.Nextcloud.client().Transport = handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, strings.NewReplacer("Bericht%202027.docx", "re%E2%80%8Bport%0Aname.txt", "Fotos/", "fol%E2%80%AEder%0Aname/").Replace(ncListing))
	})}
	for _, call := range []func() (toolResult, callMeta){
		func() (toolResult, callMeta) {
			return s.nextcloudList(context.Background(), user, json.RawMessage(`{"folder":"Dokumente"}`))
		},
		func() (toolResult, callMeta) {
			return s.nextcloudSearch(context.Background(), user, json.RawMessage(`{"term":"name"}`))
		},
	} {
		res, meta := call()
		if res.IsError || meta.status != http.StatusOK || len(res.Content) != 1 {
			t.Fatalf("listing: %+v %+v", res, meta)
		}
		text := res.Content[0].Text
		if strings.ContainsAny(text, "\u200b\u202e") || strings.Contains(text, "report\nname") || strings.Contains(text, "folder\nname") || !strings.Contains(text, "report name.txt") || !strings.Contains(text, "folder name/") {
			t.Fatalf("unsafe listing: %q", text)
		}
	}
}
