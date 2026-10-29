// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestIntegrationDisabled(t *testing.T) {
	d, err := instance.Load("../../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	// Configured dependencies and catalog grants cannot override the instance.
	s := New(Server{SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: &rec{}, Log: slog.New(slog.DiscardHandler),
		Files: &fakeFiles{}, PDF: func(context.Context, []byte, string) ([]byte, error) { panic("disabled PDF called") },
		Feedback: new(store.Store), Nextcloud: &Nextcloud{}, Rooms: &Rooms{},
		Grants: func(context.Context, string, string) (map[string]bool, map[string]string) {
			return map[string]bool{"web_search": true, "md_to_pdf": true, "rooms_free": true}, nil
		},
	})
	const me = "10.90.1.10:40000"
	for _, grants := range []bool{true, false} {
		if !grants {
			s.Grants = nil
		}
		list := rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		var result struct {
			Result struct{ Tools []json.RawMessage }
			Error  json.RawMessage
		}
		if err := json.Unmarshal(list.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Error) != 0 || len(result.Result.Tools) != 0 {
			t.Fatalf("disabled list: %s", list.Body)
		}
		for _, tool := range []string{"feedback_send", "web_search", "web_read", "md_to_pdf", "rooms_free", "image_view", "image_generate", "nextcloud_fetch", "toolbox_run"} {
			call := rpc(s, me, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, tool))
			if !strings.Contains(call.Body.String(), `"error"`) {
				t.Errorf("disabled %s callable: %s", tool, call.Body)
			}
		}
	}
	init := rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if strings.Contains(init.Body.String(), "\x7a\x6b\x6d") || !strings.Contains(init.Body.String(), `"name":"workspace"`) {
		t.Fatalf("minimal identity: %s", init.Body)
	}
}
