// SPDX-License-Identifier: MIT

package catalog

import (
	"context"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
	"log/slog"
	"slices"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestAssistantGrantsIndependentOfPersonalCatalog(t *testing.T) {
	d, err := testinstance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Select(d)()
	d.KnowledgeSources["wiki"] = assistants.Source{MCPServer: "workspace-knowledge/workspace_kb", ReadOnly: true, Instruction: "Wiki", Tools: []string{"search_workspace_wiki"}}
	s := &Service{Dir: "testdata/catalog", Store: memStore{}, Log: slog.New(slog.DiscardHandler)}
	p := assistants.Definition{KnowledgeSources: []string{"wiki"}, Skills: []string{"minutes"}, MCPTools: []string{"workspace.md_to_pdf"}}
	for _, on := range []bool{false, true, false} {
		for _, id := range []string{"workspace-knowledge", "minutes", "media", "md-to-pdf"} {
			if err := s.Store.SetOptIn(context.Background(), "u", id, on); err != nil {
				t.Fatal(err)
			}
		}
		items := s.AssistantItems(p)
		var ids []string
		for _, it := range items {
			ids = append(ids, it.ID)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, []string{"md-to-pdf", "minutes", "workspace-knowledge"}) {
			t.Fatal("wrong assistant grants", ids)
		}
		if got := s.OnItems(context.Background(), "u", KindCentral); (len(got) != 0) != on {
			t.Fatal("personal grant changed", got)
		}
		if allowed, _ := s.Grants(context.Background(), "u", ""); allowed["md_to_pdf"] != on {
			t.Fatal("tool leaked to helper", allowed)
		}
	}
	if got := s.AssistantItems(assistants.Definition{}); len(got) != 0 {
		t.Fatal("grants leaked to another assistant", got)
	}
	d.Integrations["workspace-knowledge"] = false
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, it := range s.AssistantItems(p) {
		if it.ID == "workspace-knowledge" {
			t.Fatal("disabled integration granted")
		}
	}
}
