// SPDX-License-Identifier: MIT

package instance

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/integration"
)

// Enabled reads the validated selection, not endpoint presence or person opt-ins.
func (d *Definition) Enabled(name string) bool {
	for _, m := range d.resolved {
		if m.Name == name {
			return true
		}
	}
	return false
}

// ToolEnabled gates both discovery and direct calls in the legacy MCP transport.
func (d *Definition) ToolEnabled(tool string) bool {
	for _, m := range d.resolved {
		if !m.BuiltinTools() {
			continue
		}
		for _, local := range m.Tools {
			if integration.LegacyTool(m.Name, local) == tool {
				return true
			}
		}
	}
	return false
}

func (d *Definition) HasBuiltinTools() bool {
	for _, m := range d.resolved {
		if len(m.Tools) > 0 && m.BuiltinTools() {
			return true
		}
	}
	return false
}

var condition = regexp.MustCompile(`(?m)^[ \t]*\[\[(end|if-tools|if-global-tools|if:[a-z0-9-]+)\]\][ \t]*\n`)

// conditional renders integration-owned sections without changing enabled text.
// Conditions cannot nest; malformed or unknown switches refuse instance loading.
func (d *Definition) conditional(s string) (string, error) {
	known := map[string]bool{}
	for _, m := range d.manifests {
		known[m.Name] = true
	}
	var out strings.Builder
	last, active, inside := 0, true, false
	for _, loc := range condition.FindAllStringSubmatchIndex(s, -1) {
		t := s[loc[2]:loc[3]]
		if active {
			out.WriteString(s[last:loc[0]])
		}
		last = loc[1]
		if t == "end" {
			if !inside {
				return "", fmt.Errorf("instance: unmatched conditional end")
			}
			active, inside = true, false
			continue
		}
		if inside {
			return "", fmt.Errorf("instance: nested integration condition")
		}
		inside = true
		if t == "if-tools" || t == "if-global-tools" {
			active = d.HasBuiltinTools() && (t != "if-global-tools" || !d.Capabilities.Projects)
			continue
		}
		name := strings.TrimPrefix(t, "if:")
		if !known[name] {
			return "", fmt.Errorf("instance: unknown integration condition %s", name)
		}
		active = d.Enabled(name)
	}
	if inside {
		return "", fmt.Errorf("instance: unclosed integration condition")
	}
	out.WriteString(s[last:])
	return out.String(), nil
}

// RemoteEnabled gates catalog transports independently of builtin tool names.
func (d *Definition) RemoteEnabled(name string) bool {
	for _, m := range d.resolved {
		if m.Name == name {
			return !m.BuiltinTools()
		}
	}
	return false
}

// CatalogEntry is the on-disk catalog schema, also accepted in an instance layer.
type CatalogEntry struct {
	ID            string   `json:"id" yaml:"id"`
	Kind          string   `json:"kind" yaml:"kind"`
	Title         string   `json:"title" yaml:"title"`
	TitleDE       string   `json:"title_de" yaml:"title_de"`
	Description   string   `json:"description" yaml:"description"`
	DescriptionDE string   `json:"description_de" yaml:"description_de"`
	Tier          string   `json:"tier" yaml:"tier"`
	Category      string   `json:"category" yaml:"category"`
	Server        string   `json:"server" yaml:"server"`
	Image         string   `json:"image" yaml:"image"`
	Tools         []string `json:"tools" yaml:"tools"`
	LiteLLM       []string `json:"litellm" yaml:"litellm"`
	Programs      []string `json:"programs" yaml:"programs"`
}
