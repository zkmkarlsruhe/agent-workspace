// SPDX-License-Identifier: MIT

package mcp

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestDisplayTitles(t *testing.T) {
	for _, path := range []string{"../../../../instance/example/instance.yaml", "../../../../instance/minimal/instance.yaml"} {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			d, err := instance.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(instance.Select(d))
			en, de := DisplayTitles("en"), DisplayTitles("de")
			prefix := d.MCPServer + "_"
			if en[prefix+"freecad_open"] != "Open FreeCAD" || de[prefix+"freecad_open"] != "FreeCAD öffnen" {
				t.Fatal("FreeCAD discovery titles missing or not localized", en, de)
			}
			for _, group := range [][]map[string]any{toolList, cadTools, officeTools, knowledgeTools, fileTools, nextcloudTools, nextcloudSyncTools, roomsTools} {
				for _, tool := range group {
					key := prefix + tool["name"].(string)
					if en[key] == "" || de[key] == "" || en[key] == de[key] {
						t.Errorf("missing translation for %s", key)
					}
				}
			}
			for key := range en {
				if !strings.HasPrefix(key, prefix) {
					t.Errorf("wrong instance namespace: %s", key)
				}
			}
		})
	}
}
