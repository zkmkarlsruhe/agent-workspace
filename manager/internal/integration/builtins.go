// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed builtins.json
var builtinJSON []byte

// Builtins is the shipped manifest inventory. Tests compare this embedded
// snapshot with deploy/integrations so the scratch image needs no source tree.
func Builtins() ([]Manifest, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(builtinJSON, &raw); err != nil {
		return nil, err
	}
	out := make([]Manifest, 0, len(raw))
	for _, b := range raw {
		m, err := Load(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Inventory adds operator manifests to the embedded inventory. Empty dir disables
// external loading; files are read once when the instance is loaded.
func Inventory(dir string) ([]Manifest, error) {
	out, err := Builtins()
	if err != nil || dir == "" {
		return out, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("integrations directory %s: %w", dir, err)
	}
	seen := map[string]bool{}
	for _, m := range out {
		seen[m.Name] = true
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file := filepath.Join(dir, entry.Name())
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", file, err)
		}
		m, err := Load(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", file, err)
		}
		if seen[m.Name] {
			return nil, fmt.Errorf("manifest %s: duplicate integration %q", file, m.Name)
		}
		seen[m.Name] = true
		out = append(out, m)
	}
	return out, nil
}

// LegacyTool keeps the production MCP vocabulary during the instance cutover.
// The separate-server naming contract remains available through ToolName.
func LegacyTool(integration, local string) string {
	if integration == "nextcloud" {
		return "nextcloud_" + local
	}
	return local
}
