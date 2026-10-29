// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestFreeCADGrantAndSession(t *testing.T) {
	d, err := instance.Load("../../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	granted := true
	calls := 0
	s := New(Server{SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: &rec{}, Log: slog.New(slog.DiscardHandler),
		Grants: func(context.Context, string, string) (map[string]bool, map[string]string) {
			return map[string]bool{"freecad_open": granted}, nil
		},
		FreeCAD: func(ctx context.Context, uid, session, name string, args json.RawMessage) (*sdk.CallToolResult, error) {
			calls++
			if uid != user.UID || session != "ses_12345678" {
				t.Fatal("unbound identity/session")
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ready"}}}, nil
		},
	})
	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"freecad_open","arguments":{},"_meta":{"ai.opencode/sessionID":"ses_12345678"}}}`
	list := rpc(s, "10.90.1.10:1234", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String()
	if !strings.Contains(list, `"freecad_open"`) {
		t.Fatal(list)
	}
	rpc(s, "10.90.1.10:1234", call)
	rpc(s, "10.90.1.10:1234", strings.Replace(call, `"ses_12345678"`, `""`, 1))
	rpc(s, "10.90.1.12:1234", call)
	granted = false
	rpc(s, "10.90.1.10:1234", call)
	if calls != 1 {
		t.Fatal("grant/session/source bypass", calls)
	}
	app, err := os.ReadFile("../../../../services/freecad/tools.json")
	if err != nil || !bytes.Equal(app, cadDefinitions) {
		t.Fatal("application/manager schemas differ", err)
	}
}
