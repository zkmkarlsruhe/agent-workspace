// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
)

func TestProjectCapabilitiesCentralCallGate(t *testing.T) {
	active := false
	checked := []string{}
	s := New(Server{Users: users{}, Log: slog.New(slog.DiscardHandler), ProjectScoped: true,
		SlotOf: func(netip.Addr) (int, bool) { return 1, true },
		Items: func(ctx context.Context, uid string) []catalog.Item {
			session := logx.FromContext(ctx).SessionID
			checked = append(checked, session)
			if !active || (session != "" && session != "ses_project1234") {
				return nil
			}
			return []catalog.Item{{ID: "workspace-knowledge", Kind: catalog.KindCentral}}
		}})
	rpc := func(body string) (int, string) {
		r := httptest.NewRequest("POST", "/mcp/project/wiki/workspace-knowledge", strings.NewReader(body))
		r.SetPathValue("name", "workspace-knowledge")
		r.SetPathValue("capProject", "wiki")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if status, _ := rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); status != 403 {
		t.Fatal("inactive source listed", status)
	}
	active = true
	for _, session := range []string{"", "ses_other1234", "ses_profile_12345678"} {
		status, body := rpc(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_workspace_wiki","arguments":{},"_meta":{"ai.opencode/sessionID":%q}}}`, session))
		if status != 200 || !strings.Contains(body, `"isError":true`) {
			t.Fatal("invalid session reached source", status, body)
		}
	}
	if len(checked) < 4 {
		t.Fatal("scope not checked", checked)
	}
}
