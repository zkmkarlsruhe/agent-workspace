// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestDisplayTitleFallbacks(t *testing.T) {
	for _, c := range []struct{ title, annotation, name, want string }{
		{"Readable title", "Old title", "read", "Readable title"},
		{" \n ", "Old title", "read", "Old title"},
		{"", "", "read", "read"},
		{"<img src=x onerror=alert(1)>", "", "read", "<img src=x onerror=alert(1)>"},
		{"before\u202E\n after\x00", "", "read", "before after"},
		{strings.Repeat("x", 200), "", "read", strings.Repeat("x", 160)},
	} {
		tool := &sdk.Tool{Name: c.name, Title: c.title, Annotations: &sdk.ToolAnnotations{Title: c.annotation}}
		if got := displayTitle(tool); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
		if tool.Name != c.name || tool.Title != c.title {
			t.Fatal("discovery metadata mutated")
		}
	}
}

func TestDisplayTitlesDiscoveryAndScope(t *testing.T) {
	f := newFakeLiteLLM()
	upstream := sdk.NewServer(&sdk.Implementation{Name: "titles", Version: "1"}, nil)
	for _, def := range []*sdk.Tool{
		{Name: "workspace_kb-search_workspace_wiki", Title: "Search knowledge", Annotations: &sdk.ToolAnnotations{Title: "Old title"}},
		{Name: "workspace_kb-search_workspace_werke", Annotations: &sdk.ToolAnnotations{Title: "Find artworks"}},
		{Name: "workspace_kb-search_workspace_drupal"},
		{Name: "workspace_kb-secret", Title: "Hidden title"},
	} {
		def.InputSchema = map[string]any{"type": "object"}
		upstream.AddTool(def, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			t.Error("display lookup called a tool")
			return nil, errors.New("unexpected call")
		})
	}
	f.mcp = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return upstream }, nil)
	s, _, usage := setupWith(t, true, f)
	items := s.Items
	allowed := true
	s.Items = func(ctx context.Context, uid string) []catalog.Item {
		if !allowed || logx.FromContext(ctx).SessionID != "ses_project0001" {
			return nil
		}
		return items(ctx, uid)
	}
	user := store.User{UID: "u1", Username: "alice"}
	got, err := s.DisplayTitles(context.Background(), user, "ses_project0001")
	if err != nil || len(got) != 3 || got["workspace-knowledge_search_workspace_wiki"] != "Search knowledge" || got["workspace-knowledge_search_workspace_werke"] != "Find artworks" || got["workspace-knowledge_search_workspace_drupal"] != "search_workspace_drupal" {
		t.Fatal(got, err)
	}
	for _, c := range []struct{ uid, session string }{{"u2", "ses_project0001"}, {"u1", "ses_project0002"}} {
		got, err = s.DisplayTitles(context.Background(), store.User{UID: c.uid, Username: "alice"}, c.session)
		if err != nil || len(got) != 0 {
			t.Fatal("foreign user/project labels", got, err)
		}
	}
	allowed = false
	got, err = s.DisplayTitles(context.Background(), user, "ses_project0001")
	if err != nil || len(got) != 0 {
		t.Fatal("revoked grant still returns labels", got, err)
	}
	allowed = true
	s.Assistant = func(context.Context, string) (assistants.Pin, error) { return assistants.Pin{}, errors.New("blocked") }
	got, err = s.DisplayTitles(context.Background(), user, "ses_project0001")
	if err != nil || len(got) != 0 {
		t.Fatal("source restriction bypass", got, err)
	}
	if _, err = s.DisplayTitles(context.Background(), user, "invalid"); err == nil {
		t.Fatal("invalid session accepted")
	}
	if len(usage.ev) != 0 {
		t.Fatal("discovery charged as tool invocation")
	}
}

func TestDisplayTitlesSourceFailureIsPartial(t *testing.T) {
	f := newFakeLiteLLM()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-mcp-servers") == "broken" {
			http.Error(w, "unavailable", 503)
			return
		}
		f.ServeHTTP(w, r)
	})
	s, _, _ := setupWith(t, true, handler)
	original := s.Items
	s.Items = func(ctx context.Context, uid string) []catalog.Item {
		return append(original(ctx, uid), catalog.Item{ID: "library", Kind: catalog.KindCentral, LiteLLM: []string{"broken"}, Tools: []string{"lookup"}})
	}
	got, err := s.DisplayTitles(context.Background(), store.User{UID: "u1", Username: "alice"}, "ses_project0001")
	if err != nil || got["workspace-knowledge_search_workspace_wiki"] != "search_workspace_wiki" || got["library_lookup"] != "" {
		t.Fatal(got, err)
	}
}
