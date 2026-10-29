// SPDX-License-Identifier: MIT

package instance

import (
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/integration"
)

func TestOptionalIntegrationValues(t *testing.T) {
	minimal, err := Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range strings.Fields("directory_url nextcloud_host nextcloud_root rooms_host bikes_host room_team city") {
		if _, ok := minimal.Values[key]; ok {
			t.Fatalf("minimal carries optional placeholder %s", key)
		}
	}

	for key, owner := range map[string]string{
		"nextcloud_host": "nextcloud", "nextcloud_root": "nextcloud",
		"rooms_host": "workspace-rooms", "bikes_host": "workspace-rooms",
		"room_team": "workspace-rooms", "city": "workspace-rooms", "directory_url": "directory",
	} {
		t.Run(key, func(t *testing.T) {
			d, err := Load("../../../instance/example/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			delete(d.Values, key)
			if owner == "directory" {
				d.Defaults["OC_LDAP_URL"] = "ldap://directory.example.org"
			}
			if err := d.Validate(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("enabled integration accepted missing %s: %v", key, err)
			}
			if owner == "directory" {
				delete(d.Defaults, "OC_LDAP_URL")
			} else {
				d.Integrations[owner] = false
				if owner == "nextcloud" {
					d.Integrations["office"] = false
				}
			}
			if err := d.Validate(); err != nil {
				t.Fatalf("disabled integration requires %s: %v", key, err)
			}
			for file, content := range d.Render() {
				if strings.Contains(string(content), "[["+key+"]]") {
					t.Fatalf("unresolved %s in %s", key, file)
				}
			}
			d.Values[key] = "bad\nvalue"
			if err := d.Validate(); err == nil {
				t.Fatal("invalid supplied optional value accepted")
			}
		})
	}
}

func TestIntegrationSelection(t *testing.T) {
	minimal, err := Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(minimal.resolved) != 0 || minimal.HasBuiltinTools() || minimal.ToolEnabled("feedback_send") {
		t.Fatal("minimal enabled integrations")
	}
	workspace, err := Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	builtins, err := integration.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.resolved) != len(builtins) || !workspace.Enabled("file-tools") || !workspace.HasBuiltinTools() {
		t.Fatalf("production selection: %v", workspace.resolved)
	}
	for _, tool := range []string{"feedback_send", "nextcloud_fetch", "md_to_pdf", "image_view", "web_search", "rooms_free", "toolbox_run"} {
		if !workspace.ToolEnabled(tool) {
			t.Errorf("production lost %s", tool)
		}
	}
	for _, name := range []string{"feedback", "nextcloud", "images", "web-research", "workspace-rooms", "evaluation", "workspace-style", "office"} {
		workspace.Integrations[name] = false
	}
	if err := workspace.Validate(); err != nil {
		t.Fatal(err)
	}
	out := workspace.Render()
	for _, filename := range []string{"AGENTS.md", "agents/helper.md"} {
		for _, section := range []string{"## Nextcloud", "## Rooms, bikes", "## Feedback", "- Feedback:", "- Prototype:", "[[if:"} {
			if strings.Contains(string(out[filename]), section) {
				t.Errorf("disabled section %q in %s", section, filename)
			}
		}
	}
	for name := range workspace.Integrations {
		workspace.Integrations[name] = false
	}
	if err := workspace.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(workspace.Render()["opencode.jsonc"]), `"url": "http://llm-gw:4000/mcp/`) {
		t.Fatal("disabled MCP still rendered")
	}
}

func TestFileToolsMinimalRuntime(t *testing.T) {
	d, err := Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d.Integrations["file-tools"] = true
	d.IntegrationGrants = map[string][]string{"file-tools": {"workspace:read", "workspace:write"}}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	rendered := string(d.Render()["opencode.jsonc"])
	for _, value := range []string{"http://llm-gw:4000/mcp/workspace", `"execution": 240000`, `"resource": "mcp:workspace", "effect": "allow"`} {
		if !strings.Contains(rendered, value) {
			t.Fatalf("missing runtime contract %s", value)
		}
	}
}

func TestIntegrationValidation(t *testing.T) {
	for name, change := range map[string]func(*Definition){
		"unknown":    func(d *Definition) { d.Integrations["typo"] = false },
		"dependency": func(d *Definition) { d.Integrations["pdf"] = false },
		"grant":      func(d *Definition) { delete(d.IntegrationGrants, "evaluation") },
		"server":     func(d *Definition) { d.MCPServer = "../bad" },
		"condition":  func(d *Definition) { d.Artifacts["AGENTS.md"] = "[[if:typo]]\nx\n[[end]]\n" },
		"unclosed":   func(d *Definition) { d.Artifacts["AGENTS.md"] = "[[if:pdf]]\nx\n" },
		"inline":     func(d *Definition) { d.Artifacts["AGENTS.md"] = "x [[if:pdf]]\n" },
		"nested":     func(d *Definition) { d.Artifacts["AGENTS.md"] = "[[if:pdf]]\n[[if:images]]\nx\n[[end]]\n[[end]]\n" },
	} {
		t.Run(name, func(t *testing.T) {
			d, err := Load("../../../instance/example/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			change(d)
			if err := d.Validate(); err == nil {
				t.Fatal("invalid integration selection accepted")
			}
		})
	}
}
