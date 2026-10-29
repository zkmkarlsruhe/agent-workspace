// SPDX-License-Identifier: MIT

package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestToolImageFromEnv(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "skills"), 0o755)
	ref := "registry.example.org/operations/opencode-tool-medien:2026.10.18.9@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"items":[{"id":"media","kind":"toolbox","tier":"optional","title":"Media",
		"image":"registry.example.org/operations/opencode-tool-medien:2026.10.18.1@sha256:06f8c80e73d7ea2217e3fced10b67b13eef3fef59d1009deec11f576b39a30ea","programs":["ffmpeg"]}]}`), 0o644)
	t.Setenv("OC_TOOL_IMAGE_MEDIA", ref)
	items, _ := Load(dir)
	for _, it := range items {
		if it.ID == "media" {
			if it.Image != ref {
				t.Fatalf("image %q, want env %q", it.Image, ref)
			}
			return
		}
	}
	t.Fatal("media missing")
}
