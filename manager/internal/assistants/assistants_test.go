// SPDX-License-Identifier: MIT

package assistants

import (
	"bytes"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func example() Definition {
	return Definition{ID: "support", Version: 1, Title: "Support", Description: "Support tasks", ModelRole: "small", ConsentMode: "pending",
		Prompt:   Prompt{Role: "Help using {{tool knowledge.knowledge_read}}.", Rules: []string{"Read sources."}, Escalation: "Draft a request."},
		MCPTools: []string{"knowledge.knowledge_read"}, Skills: []string{"docs"}, BuiltinTools: []string{"question"}, EntryQuestion: "What failed?"}
}

func TestValidation(t *testing.T) {
	for name, change := range map[string]func(*Definition){
		"icon":             func(p *Definition) { p.Icon = "https://example.org/icon.svg" },
		"translation":      func(p *Definition) { p.DescriptionEN = "bad\x00" },
		"path":             func(p *Definition) { p.ID = "../escape" },
		"revision":         func(p *Definition) { p.Version = 0 },
		"model":            func(p *Definition) { p.ModelRole = "provider/key" },
		"consent":          func(p *Definition) { p.ConsentMode = "approved" },
		"source wildcard":  func(p *Definition) { p.KnowledgeSources = []string{"*"} },
		"source duplicate": func(p *Definition) { p.KnowledgeSources = []string{"docs", "docs"} },
		"skill path":       func(p *Definition) { p.Skills = []string{"../docs"} },
		"tool wildcard":    func(p *Definition) { p.MCPTools = append(p.MCPTools, "knowledge.*") },
		"tool collision":   func(p *Definition) { p.MCPTools = append(p.MCPTools, "a_b.c", "a.b_c") },
		"ungranted prompt": func(p *Definition) { p.MCPTools = nil },
		"bad template":     func(p *Definition) { p.Prompt.Role = "{{tool bad}" },
		"blank rule":       func(p *Definition) { p.Prompt.Rules = []string{" "} },
		"invalid entry":    func(p *Definition) { p.EntryQuestion = "hello\x00" },
		"long init":        func(p *Definition) { p.InitPrompt = strings.Repeat("x", 16001) },
		"subagent":         func(p *Definition) { p.BuiltinTools = []string{"subagent"} },
		"code mode":        func(p *Definition) { p.BuiltinTools = []string{"execute"} },
	} {
		t.Run(name, func(t *testing.T) {
			p := example()
			change(&p)
			if p.Validate() == nil {
				t.Fatal("accepted invalid definition")
			}
		})
	}
}

func TestOptionalPresentationNeverRendered(t *testing.T) {
	p := example()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	before := p.Pin("model")
	p.EntryQuestion = "Question <script> & UI-only text"
	p.InitPrompt = "Initialize this conversation"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{p.EntryQuestion, p.InitPrompt, "## Start questions"} {
		if strings.Contains(string(p.RenderAgent()), text) {
			t.Fatalf("presentation leaked into runtime instructions: %q", text)
		}
	}
	if before == p.Pin("model") {
		t.Fatal("presentation is not pinned")
	}
	p.EntryQuestion, p.InitPrompt = " \n", "\t"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRenderPreviewAndPin(t *testing.T) {
	p := example()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	b := p.RenderAgent()
	if !bytes.Equal(b, p.RenderAgent()) {
		t.Fatal("nondeterministic renderer")
	}
	parts := strings.SplitN(string(b), "---\n", 3)
	var fm struct {
		Mode, Model string
		Disabled    bool
		Permissions []Rule
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &fm); err != nil {
		t.Fatal(err)
	}
	if fm.Mode != "primary" || fm.Model != "litellm/fast" || !fm.Disabled {
		t.Fatalf("unsafe preview: %+v", fm)
	}
	if fm.Permissions[0] != (Rule{"*", "*", "deny"}) {
		t.Fatal("missing default deny")
	}
	for _, r := range fm.Permissions[1:] {
		if r.Action == "shell" || r.Action == "edit" || r.Action == "subagent" {
			t.Fatal("ungranted builtin enabled")
		}
	}
	if strings.Contains(parts[2], "{{") || !strings.Contains(parts[2], "knowledge_knowledge_read") {
		t.Fatal("unresolved tool")
	}
	pin := p.Pin("model-a")
	if err := pin.Validate(); err != nil {
		t.Fatal(err)
	}
	if pin != p.Pin("model-a") || pin == p.Pin("model-b") {
		t.Fatal("model not pinned")
	}
	p.KnowledgeSources = []string{"different-scope"}
	if pin == p.Pin("model-a") {
		t.Fatal("sources not pinned")
	}
	p = example()
	p.Prompt.Rules = append(p.Prompt.Rules, "Escalate sooner.")
	if pin == p.Pin("model-a") {
		t.Fatal("prompt not pinned")
	}
}

func TestEnabledSupportSubset(t *testing.T) {
	p := example()
	p.ConsentMode = "support"
	p.Enabled = true
	p.Skills = nil
	p.MCPTools = nil
	p.Prompt.Role = "Read approved support sources."
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Definition){
		func(p *Definition) { p.BuiltinTools = []string{"question", "shell"} },
		func(p *Definition) { p.Skills = []string{"docs"} },
		func(p *Definition) { p.MCPTools = []string{"other.read"} },
		func(p *Definition) { p.ConsentMode = "pending" },
	} {
		bad := p
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("enabled unsupported execution", bad)
		}
	}
	if !p.Available(nil) {
		t.Fatal("all-account profile hidden")
	}
	p.Audience = []string{"support"}
	if p.Available(nil) || !p.Available([]string{"support"}) {
		t.Fatal("audience not enforced")
	}
	before := p.Pin("model")
	p.Evaluation = true
	if before == p.Pin("model") {
		t.Fatal("evaluation terms not pinned")
	}
}

func TestResearchOptions(t *testing.T) {
	p := example()
	p.Enabled, p.ConsentMode, p.Skills = true, "support", nil
	p.Prompt.Role = "Research with sources."
	p.MCPTools = []string{"workspace.web_search", "workspace.web_read"}
	p.BuiltinTools = []string{"question", "read", "write"}
	for _, mode := range []string{"", "per_task", "shared"} {
		p.ProjectMode = mode
		if err := p.Validate(); err != nil {
			t.Fatal(mode, err)
		}
	}
	before := p.Pin("model")
	p.ProjectMode = "per_task"
	if before == p.Pin("model") {
		t.Fatal("mode not pinned")
	}
	p.ProjectMode = "other"
	if p.Validate() == nil {
		t.Fatal("invalid mode accepted")
	}
	for title, want := range map[string]string{"IT-Hilfe": "it-hilfe", "Bücher & Medien": "buecher-medien", "???": p.ID} {
		p.Title = title
		if got := p.SharedFolder(); got != want {
			t.Fatal(title, got, want)
		}
	}
}

func TestBuiltInIcons(t *testing.T) {
	for _, icon := range []string{"", "chat", "tools", "search", "book"} {
		p := example()
		p.Icon = icon
		if err := p.Validate(); err != nil {
			t.Fatalf("%q: %v", icon, err)
		}
	}
}
