// SPDX-License-Identifier: MIT

package instance

import (
	"strings"
	"testing"
)

func TestCapabilitiesOptions(t *testing.T) {
	original := string(builtin)
	for _, value := range []string{"all", "none", "[]", "[workspace-knowledge, web-research]"} {
		d, err := Parse([]byte(strings.Replace(original, "project_default: [workspace-knowledge, web-research]", "project_default: "+value, 1)))
		if err != nil || !d.Capabilities.Projects {
			t.Fatalf("%s: %v", value, err)
		}
	}
	for _, value := range []string{"false", "typo", "[rooms, rooms]", "[../rooms]", "{rooms: true}"} {
		if _, err := Parse([]byte(strings.Replace(original, "project_default: [workspace-knowledge, web-research]", "project_default: "+value, 1))); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	d, err := Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(d.Render()["opencode.jsonc"]), `"url": "http://llm-gw:4000/mcp/workspace"`) {
		t.Fatal("managed server overrides project URL")
	}
	d.Capabilities.Projects = false
	if !strings.Contains(string(d.Render()["opencode.jsonc"]), `"url": "http://llm-gw:4000/mcp/workspace"`) {
		t.Fatal("off switch lost global server")
	}
	d.Capabilities.LegacyDefault = "bad"
	if err := d.Validate(); err == nil {
		t.Fatal("invalid migration default")
	}
}
