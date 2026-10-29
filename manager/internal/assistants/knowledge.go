// SPDX-License-Identifier: MIT

package assistants

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// ProjectConfig loads sources only in this project's location. Gateway calls
// still validate the stored binding and session; the editable file grants nothing.
func ProjectConfig(project string, sources []Source, mcpTools ...string) []byte {
	servers := map[string]any{}
	entries := []string{}
	for _, source := range sources {
		entry, _ := source.Server()
		entries = append(entries, entry)
	}
	for _, tool := range mcpTools {
		entry, _, _ := strings.Cut(tool, ".")
		entries = append(entries, entry)
	}
	for _, entry := range entries {
		servers[entry] = map[string]any{"type": "remote", "url": "http://llm-gw:4000/mcp/assistant/" + url.PathEscape(project) + "/" + entry,
			"oauth": false, "codemode": false, "timeout": map[string]int{"startup": 10000, "catalog": 15000, "execution": 60000}}
	}
	b, _ := json.Marshal(map[string]any{"mcp": map[string]any{"servers": servers}})
	return b
}

// Source references an existing MCP, never a backend API or credentials.
// MCPServer is <catalog entry>/<upstream server> for the existing central gateway.
type Source struct {
	MCPServer   string   `yaml:"mcp_server" json:"mcp_server"`
	ReadOnly    bool     `yaml:"read_only" json:"read_only"`
	Instruction string   `yaml:"instruction" json:"instruction"`
	Tools       []string `yaml:"tools,omitempty" json:"tools"`
}

type Sources map[string]Source

var sourceIDRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var nativeToolRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func (s Source) Server() (entry, upstream string) {
	entry, upstream, _ = strings.Cut(s.MCPServer, "/")
	return
}

func (sources Sources) Validate() error {
	refs := map[string]bool{}
	for id, s := range sources {
		entry, upstream := s.Server()
		if !sourceIDRE.MatchString(id) || !idRE.MatchString(entry) || !nativeToolRE.MatchString(upstream) || refs[s.MCPServer] {
			return fmt.Errorf("knowledge source %s: invalid or duplicate MCP reference", id)
		}
		refs[s.MCPServer] = true
		if !s.ReadOnly || strings.TrimSpace(s.Instruction) == "" || strings.ContainsAny(s.Instruction, "\r\n\x00") || strings.Contains(s.Instruction, "{{") {
			return fmt.Errorf("knowledge source %s: read_only and a single-line instruction required", id)
		}
		seen := map[string]bool{}
		for _, name := range s.Tools {
			if !nativeToolRE.MatchString(name) || seen[name] {
				return fmt.Errorf("knowledge source %s: invalid or duplicate tool %s", id, name)
			}
			seen[name] = true
		}
	}
	return nil
}

// Allows uses reviewed allowlists for legacy servers without annotations. An
// explicit write annotation always denies; unknown tools require readOnlyHint.
func (s Source) Allows(name string, readOnly *bool) bool {
	if !s.ReadOnly || (readOnly != nil && !*readOnly) {
		return false
	}
	if s.Tools != nil {
		return slices.Contains(s.Tools, name)
	}
	return readOnly != nil && *readOnly
}

func (p Definition) Sources(registry Sources) ([]Source, error) {
	out := make([]Source, 0, len(p.KnowledgeSources))
	for _, id := range p.KnowledgeSources {
		s, ok := registry[id]
		if !ok {
			return nil, fmt.Errorf("assistant %s: unknown knowledge source %s", p.ID, id)
		}
		out = append(out, s)
	}
	return out, nil
}
