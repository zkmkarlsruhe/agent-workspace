// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"log/slog"
	"strings"
	"testing"
)

func TestOfficeNeedsInstanceAndProjectGrantOnEveryCall(t *testing.T) {
	d, err := instance.Load("../../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d.Integrations["office"] = true
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	granted := true
	calls := 0
	s := New(Server{SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: &rec{}, Log: slog.New(slog.DiscardHandler),
		Grants: func(context.Context, string, string) (map[string]bool, map[string]string) {
			return map[string]bool{"office_open": granted}, nil
		},
		Office: func(ctx context.Context, uid, name string, args json.RawMessage) (*sdk.CallToolResult, error) {
			calls++
			if uid != user.UID {
				t.Fatal("identity not taken from authenticated source")
			}
			return &sdk.CallToolResult{StructuredContent: map[string]any{"documentId": "one"}, Content: []sdk.Content{&sdk.TextContent{Text: "opened"}}}, nil
		},
	})
	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"office_open","arguments":{"fileId":"1"}}}`
	list := rpc(s, "10.90.1.10:1234", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(list.Body.String(), `"office_open"`) {
		t.Fatal(list.Body.String())
	}
	if body := rpc(s, "10.90.1.10:1234", call).Body.String(); !strings.Contains(body, `"documentId":"one"`) {
		t.Fatal(body)
	}
	granted = false
	rpc(s, "10.90.1.10:1234", call)
	granted = true
	s.Grants = nil
	rpc(s, "10.90.1.10:1234", call)
	rpc(s, "10.90.1.11:1234", call)
	if calls != 1 {
		t.Fatal("bypassed capability or source identity")
	}
}
func TestNextcloudListingCarriesValidatedFileID(t *testing.T) {
	body := `<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/files/alice/Report.docx</d:href><d:propstat><d:prop><oc:fileid>1234</oc:fileid></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`
	for _, id := range []string{"1234", "../unsafe", "-1", "0"} {
		entries, err := parseListing(strings.NewReader(strings.Replace(body, "1234", id, 1)), "/files/alice", "")
		if err != nil || len(entries) != 1 {
			t.Fatal(err, entries)
		}
		want := ""
		if id == "1234" {
			want = id
		}
		if entries[0].FileID != want {
			t.Fatal("unvalidated file id")
		}
	}
}
