// SPDX-License-Identifier: MIT

package assistants

import (
	"encoding/json"
	"strings"
	"testing"
)

func source() Source {
	return Source{MCPServer: "wiki/outline", ReadOnly: true, Instruction: "Internal guidance; use for support.", Tools: []string{"documents.search", "get-page"}}
}

func TestSourceValidation(t *testing.T) {
	for name, change := range map[string]func(Sources){
		"invalid id":        func(r Sources) { r["../x"] = source() },
		"duplicate server":  func(r Sources) { r["another"] = source() },
		"missing reference": func(r Sources) { s := r["wiki"]; s.MCPServer = ""; r["wiki"] = s },
		"URL":               func(r Sources) { s := r["wiki"]; s.MCPServer = "https://secret@example.org"; r["wiki"] = s },
		"write":             func(r Sources) { s := r["wiki"]; s.ReadOnly = false; r["wiki"] = s },
		"blank":             func(r Sources) { s := r["wiki"]; s.Instruction = " "; r["wiki"] = s },
		"multiline":         func(r Sources) { s := r["wiki"]; s.Instruction = "foo\nbar"; r["wiki"] = s },
		"wildcard":          func(r Sources) { s := r["wiki"]; s.Tools = []string{"*"}; r["wiki"] = s },
		"duplicate tool":    func(r Sources) { s := r["wiki"]; s.Tools = []string{"fetch", "fetch"}; r["wiki"] = s },
	} {
		t.Run(name, func(t *testing.T) {
			r := Sources{"wiki": source()}
			if err := r.Validate(); err != nil {
				t.Fatal(err)
			}
			change(r)
			if r.Validate() == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
}

func TestSourceReadOnly(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name  string
		tools []string
		hint  *bool
		want  bool
	}{
		{"reviewed legacy", []string{"odd-name"}, nil, true},
		{"reviewed read", []string{"odd-name"}, &yes, true},
		{"write overrides allowlist", []string{"odd-name"}, &no, false},
		{"unlisted", []string{"other"}, &yes, false},
		{"explicit empty", []string{}, &yes, false},
		{"discovered read", nil, &yes, true},
		{"discovered write", nil, &no, false},
		{"unknown", nil, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := source()
			s.Tools = c.tools
			if got := s.Allows("odd-name", c.hint); got != c.want {
				t.Fatalf("allowed %v", got)
			}
		})
	}
}

func TestSourceRenderAndPin(t *testing.T) {
	p := example()
	p.KnowledgeSources = []string{"wiki"}
	s := source()
	sources, err := p.Sources(Sources{"wiki": s})
	if err != nil {
		t.Fatal(err)
	}
	b := string(p.RenderAgent(sources...))
	for _, text := range []string{s.Instruction, s.MCPServer, "Always answer with source links", "wiki_documents.search"} {
		if !strings.Contains(b, text) {
			t.Fatalf("missing %s", text)
		}
	}
	pin := p.Pin("model", s)
	s.Instruction = "Changed scope."
	if pin == p.Pin("model", s) {
		t.Fatal("source instruction not pinned")
	}
	s = source()
	s.Tools = nil
	if pin == p.Pin("model", s) {
		t.Fatal("source tools not pinned")
	}
	empty := s
	empty.Tools = []string{}
	if p.Pin("model", s) == p.Pin("model", empty) {
		t.Fatal("absent and empty allowlists have the same pin")
	}
	if _, err := p.Sources(nil); err == nil {
		t.Fatal("unknown source accepted")
	}
}

func TestProjectConfigOrganisationTools(t *testing.T) {
	for _, grant := range []bool{false, true} {
		var tools []string
		if grant {
			tools = []string{"workspace.web_search", "workspace.web_read"}
		}
		var config struct {
			MCP struct {
				Servers map[string]struct{ URL string }
			}
		}
		if err := json.Unmarshal(ProjectConfig("project-id", []Source{source()}, tools...), &config); err != nil {
			t.Fatal(err)
		}
		if config.MCP.Servers["wiki"].URL != "http://llm-gw:4000/mcp/assistant/project-id/wiki" {
			t.Fatal(config)
		}
		org, exists := config.MCP.Servers["workspace"]
		if exists != grant || (exists && org.URL != "http://llm-gw:4000/mcp/assistant/project-id/workspace") {
			t.Fatal(config)
		}
	}
}
