// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
)

func TestHiddenTools(t *testing.T) {
	on := false
	r := &rec{}
	s := New(Server{SearXNG: mustURL("http://127.0.0.1:1"), SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf,
		Users: users{}, Usage: r, Log: slog.New(slog.DiscardHandler), Files: &fakeFiles{},
		PDF:       func(context.Context, []byte, string) ([]byte, error) { return []byte("%PDF"), nil },
		Nextcloud: &Nextcloud{Base: mustURL("https://cloud.example")},
		// web_read is granted by no item: deny-by-default
		Grants: func(_ context.Context, uid, _ string) (map[string]bool, map[string]string) {
			allowed := map[string]bool{"web_search": true}
			if on && uid == "u1" {
				allowed["md_to_pdf"] = true
			}
			return allowed, map[string]string{"md_to_pdf": "Ein PDF erstellen"}
		}})
	const me = "10.90.1.10:40000"
	list := func() string { return rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String() }
	if l := list(); strings.Contains(l, `"md_to_pdf"`) || !strings.Contains(l, `"web_search"`) || strings.Contains(l, `"web_read"`) ||
		!strings.Contains(l, `"nextcloud_list"`) {
		t.Fatalf("off: %s", l)
	}
	w := rpc(s, me, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"md_to_pdf","arguments":{"file":"a.md"}}}`)
	var call struct {
		Result toolResult `json:"result"`
	}
	json.Unmarshal(w.Body.Bytes(), &call)
	if !call.Result.IsError || !strings.Contains(call.Result.Content[0].Text, "„Ein PDF erstellen“ ist ausgeschaltet") ||
		!strings.HasSuffix(call.Result.Content[0].Text, "im Portal unter Fähigkeiten einschalten.") || len(r.ev) != 0 {
		t.Fatalf("call while off: %s, events %d", w.Body, len(r.ev))
	}
	w = rpc(s, me, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_read","arguments":{"url":"https://example.org"}}}`)
	if b := w.Body.String(); !strings.Contains(b, `"error"`) || strings.Contains(b, `"result"`) || len(r.ev) != 0 {
		t.Fatalf("ungranted call: %s, events %d", b, len(r.ev))
	}
	on = true
	if l := list(); !strings.Contains(l, `"md_to_pdf"`) {
		t.Fatalf("on: %s", l)
	}
}
