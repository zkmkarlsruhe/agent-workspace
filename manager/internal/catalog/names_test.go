// SPDX-License-Identifier: MIT

package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogBrokerNames(t *testing.T) {
	for _, field := range []string{"litellm", "tools"} {
		for _, name := range []string{"valid_name-1", "", "srv,other", "bad\r\nheader", "has space", "UPPER", "name.tool", "path/tool", "ü"} {
			t.Run(field+"/"+name, func(t *testing.T) {
				item := map[string]any{"id": "test-source", "kind": "mcp-central", "tier": "optional", "title": "Source", "tools": []string{"lookup"}, "litellm": []string{"srv"}}
				item[field] = []string{name}
				raw, _ := json.Marshal(map[string]any{"items": []any{item}})
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "mcp.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				items, err := Load(dir)
				if name == "valid_name-1" {
					if err != nil || len(items) != 1 {
						t.Fatalf("valid names rejected: %v %v", items, err)
					}
				} else if err == nil || len(items) != 0 {
					t.Fatalf("invalid names accepted: %v %v", items, err)
				}
			})
		}
	}
}
