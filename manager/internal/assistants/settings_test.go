// SPDX-License-Identifier: MIT

package assistants

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func ptr[T any](v T) *T { return &v }

func TestModelSettingsValidation(t *testing.T) {
	for name, change := range map[string]func(*Definition){
		"negative temperature": func(p *Definition) { p.Temperature = ptr(-0.1) },
		"high temperature":     func(p *Definition) { p.Temperature = ptr(2.01) },
		"nan temperature":      func(p *Definition) { p.Temperature = ptr(math.NaN()) },
		"infinite top_p":       func(p *Definition) { p.TopP = ptr(math.Inf(1)) },
		"negative top_p":       func(p *Definition) { p.TopP = ptr(-0.1) },
		"high top_p":           func(p *Definition) { p.TopP = ptr(1.01) },
		"zero steps":           func(p *Definition) { p.Steps = ptr(Integer(0)) },
		"negative steps":       func(p *Definition) { p.Steps = ptr(Integer(-1)) },
		"high steps":           func(p *Definition) { p.Steps = ptr(Integer(1001)) },
		"zero output":          func(p *Definition) { p.MaxOutputTokens = ptr(Integer(0)) },
		"negative output":      func(p *Definition) { p.MaxOutputTokens = ptr(Integer(-1)) },
		"high output":          func(p *Definition) { p.MaxOutputTokens = ptr(Integer(32769)) },
		"reasoning boolean":    func(p *Definition) { p.ReasoningEffort = "on" },
		"unknown effort":       func(p *Definition) { p.ReasoningEffort = "ultra" },
		"invalid color":        func(p *Definition) { p.Color = "red" },
	} {
		t.Run(name, func(t *testing.T) {
			p := example()
			change(&p)
			if p.Validate() == nil {
				t.Fatal("invalid option accepted")
			}
		})
	}
	for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		p := example()
		p.ReasoningEffort = effort
		p.Temperature = ptr(0.0)
		p.TopP = ptr(0.0)
		p.Steps = ptr(Integer(1))
		p.MaxOutputTokens = ptr(Integer(1))
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		p.Temperature = ptr(2.0)
		p.TopP = ptr(1.0)
		p.Steps = ptr(Integer(1000))
		p.MaxOutputTokens = ptr(Integer(32768))
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRenderModelSettingsAndSystemPrompt(t *testing.T) {
	p := example()
	before := p.Pin("alias")
	p.Temperature = ptr(0.0)
	p.TopP = ptr(0.9)
	p.MaxOutputTokens = ptr(Integer(2048))
	p.ReasoningEffort = "low"
	p.Steps = ptr(Integer(8))
	p.Hidden = true
	p.Color = "#aBc123"
	p.InitPrompt = "FIRST MESSAGE ONLY"
	agent := string(p.RenderAgent())
	parts := strings.SplitN(agent, "---\n", 3)
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(parts[1]), &fm); err != nil {
		t.Fatal(err)
	}
	if fm["model"] != "litellm/fast#profile-support" || fm["steps"] != 8 || fm["hidden"] != true || fm["color"] != "#aBc123" {
		t.Fatal(fm)
	}
	for _, key := range []string{"temperature", "top_p", "max_tokens", "reasoning_effort", "request", "system", "init_prompt"} {
		if _, ok := fm[key]; ok {
			t.Fatalf("unsupported or ineffective frontmatter %s", key)
		}
	}
	if strings.Contains(agent, p.InitPrompt) || !strings.Contains(parts[2], p.Prompt.Escalation) || !strings.Contains(parts[2], "Help using knowledge_knowledge_read.") {
		t.Fatal("system and first-message prompts mixed", agent)
	}
	want := map[string]any{"temperature": 0.0, "top_p": 0.9, "max_tokens": 2048, "reasoning_effort": "low"}
	if !reflect.DeepEqual(p.ModelBody(), want) {
		t.Fatal(p.ModelBody())
	}
	if before == p.Pin("alias") {
		t.Fatal("model settings not pinned")
	}
	baseline := p.Pin("alias")
	p.Temperature = ptr(0.1)
	if baseline == p.Pin("alias") {
		t.Fatal("sampling change did not change pin")
	}
	p = example()
	if p.Model() != "litellm/fast" || len(p.ModelBody()) != 0 {
		t.Fatal("unset options changed defaults")
	}
}
