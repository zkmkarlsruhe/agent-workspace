// SPDX-License-Identifier: MIT

package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectoryManifestInstance(t *testing.T) {
	dir := "../../../deploy/example/mcp-hello/integrations"
	b, err := os.ReadFile("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), "integrations:\n", "integrations:\n  hello: true\n", 1))
	file := filepath.Join(t.TempDir(), "instance.yaml")
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithIntegrations(file, ""); err == nil {
		t.Fatal("unknown integration accepted")
	}
	d, err := LoadWithIntegrations(file, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !d.RemoteEnabled("hello") || d.HasBuiltinTools() || d.ToolEnabled("hello") {
		t.Fatal("remote tool classified as builtin")
	}
	d.Artifacts["AGENTS.md"] += "\n[[if:hello]]\nHello enabled\n[[end]]\n"
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d.Render()["AGENTS.md"]), "Hello enabled") {
		t.Fatal("operator manifest condition not rendered")
	}
	d.Integrations["hello"] = false
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.RemoteEnabled("hello") || strings.Contains(string(d.Render()["AGENTS.md"]), "Hello enabled") {
		t.Fatal("off switch ignored")
	}
	// Renderer Load honors the same environment option; the snapshot survives removal.
	t.Setenv("OC_INTEGRATIONS_DIR", dir)
	d, err = Load(file)
	if err != nil || !d.RemoteEnabled("hello") {
		t.Fatalf("environment: %v %v", d, err)
	}
	t.Setenv("OC_INTEGRATIONS_DIR", "/missing")
	if err := d.Validate(); err != nil {
		t.Fatalf("snapshot reread directory: %v", err)
	}
}

func TestOperatorCannotGrantLegacyTools(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"schema":1,"name":"addon","version":"1.0.0","runs":"service","mcp":"/mcp","tools":["web_read","web_search"]}`
	if err := os.WriteFile(filepath.Join(dir, "addon.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "integrations:\n", "integrations:\n  addon: true\n", 1))
	file := filepath.Join(t.TempDir(), "instance.yaml")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	d, err := LoadWithIntegrations(file, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !d.RemoteEnabled("addon") || d.Enabled("web-research") || d.ToolEnabled("web_read") || d.ToolEnabled("web_search") {
		t.Fatal("operator manifest granted builtin dispatch")
	}
}
