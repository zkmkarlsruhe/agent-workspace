// SPDX-License-Identifier: MIT

package catalog

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestHelloCatalogOverlay(t *testing.T) {
	d, err := instance.LoadWithIntegrations("../../../instance/minimal/instance.yaml", "../../../deploy/example/mcp-hello/integrations")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	dir := t.TempDir()
	b, err := os.ReadFile("../../../deploy/example/mcp-hello/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	items, err := Load(dir)
	if err != nil || len(items) != 1 || items[0].ID != "hello" || items[0].LiteLLM[0] != "hello" || items[0].Tools[0] != "hello" {
		t.Fatalf("overlay: %v %v", items, err)
	}
	svc := &Service{Dir: dir, Log: slog.New(slog.DiscardHandler)}
	if len(svc.Items()) != 0 {
		t.Fatal("hello on by default")
	}
	d.Integrations["hello"] = true
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := svc.Items(); len(got) != 1 || got[0].Kind != KindCentral {
		t.Fatalf("enabled: %v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "duplicate id hello") {
		t.Fatalf("collision: %v", err)
	}
}
