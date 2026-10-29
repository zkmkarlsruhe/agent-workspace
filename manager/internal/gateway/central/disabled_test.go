// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestDisabledWithoutUpstream(t *testing.T) {
	recorded := &rec{}
	s := New(Server{Users: users{}, Usage: recorded, Log: slog.New(slog.DiscardHandler),
		SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf,
		Items: func(context.Context, string) []catalog.Item {
			return []catalog.Item{{ID: "workspace-knowledge", Kind: catalog.KindCentral, Tools: []string{"lookup"}}}
		}})
	for _, tc := range []struct {
		from, name, body string
		status           int
	}{
		{"192.0.2.1:1", "workspace-knowledge", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, http.StatusForbidden},
		{me, "ungranted", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, http.StatusForbidden},
		{me, "workspace-knowledge", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, http.StatusOK},
		{me, "workspace-knowledge", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ungranted"}}`, http.StatusOK},
		{me, "workspace-knowledge", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, http.StatusServiceUnavailable},
		{me, "workspace-knowledge", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`, http.StatusServiceUnavailable},
	} {
		w := rpc(s, tc.from, tc.name, tc.body)
		if w.Code != tc.status {
			t.Errorf("%s %s: %d %s", tc.name, tc.body, w.Code, w.Body.String())
		}
	}
	if len(recorded.ev) != 1 || recorded.ev[0].Status != http.StatusServiceUnavailable {
		t.Fatalf("disabled call usage: %+v", recorded.ev)
	}
	if _, err := s.connect(context.Background(), store.User{}, catalog.Item{}); err == nil || !strings.Contains(err.Error(), "integration disabled") {
		t.Fatalf("disabled internal connection: %v", err)
	}
}
