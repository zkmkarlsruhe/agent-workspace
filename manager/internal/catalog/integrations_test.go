// SPDX-License-Identifier: MIT

package catalog

import (
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"log/slog"
	"testing"
)

func TestIntegrationCatalog(t *testing.T) {
	s := &Service{Dir: "testdata/catalog", Log: slog.New(slog.DiscardHandler)}
	if len(s.Items()) != 6 {
		t.Fatalf("production catalog changed: %v", s.Items())
	}
	d, err := instance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the transport name so only integration selection can remove entries.
	d.MCPServer = "workspace"
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	for _, it := range s.Items() {
		if it.Kind != KindSkill {
			t.Errorf("disabled integration in catalog: %+v", it)
		}
	}
}
