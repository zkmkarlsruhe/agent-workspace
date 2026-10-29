// SPDX-License-Identifier: MIT

package instance

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
)

func TestAssistantSettingsLoaderAndVariants(t *testing.T) {
	for _, bad := range []string{"temperature: -0.1", "temperature: 2.1", "temperature: .nan", "temperature: .inf", "top_p: 1.01", "steps: 0", "steps: 1.5", "steps: '8'", "hidden: invalid", "reasoning: false", "max_output_tokens: 1.5", "max_output_tokens: 32769", "reasoning_effort: impossible", "color: red", "system: silently-ignored", "request: {body: {model: bypass}}"} {
		t.Run(bad, func(t *testing.T) {
			text := strings.Replace(string(builtin), "    model_role: default\n", "    model_role: default\n    "+bad+"\n", 1)
			// Remove the configured defaults to avoid testing duplicate YAML keys instead of values.
			text = strings.Replace(text, "    temperature: 0.2\n", "", 1)
			text = strings.Replace(text, "    top_p: 1\n", "", 1)
			text = strings.Replace(text, "    steps: 20\n", "", 1)
			text = strings.Replace(text, "    max_output_tokens: 2048\n", "", 1)
			if _, err := Parse([]byte(text)); err == nil {
				t.Fatal("invalid instance accepted")
			}
		})
	}
	d, err := Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p := d.Assistants[0]
	if *p.Temperature != 0.2 || *p.Steps != 20 || *p.MaxOutputTokens != 2048 {
		t.Fatal("pilot settings missing", p)
	}
	// Use a small valid JSON template to exercise the renderer's raw JSON slots.
	d.Artifacts["opencode.jsonc"] = `{"providers":{"litellm":{"models":{"standard":{[[profiles.default_variants]]"modelID":"[[models.default]]"},"fast":{[[profiles.small_variants]]"modelID":"[[models.small]]"},"thorough":{[[profiles.thorough_variants]]"modelID":"[[models.thorough]]"}}}}}`
	second := p
	second.ID = "second"
	second.ModelRole = "small"
	second.ReasoningEffort = "none"
	third := p
	third.ID = "third"
	temperature := 0.7
	third.Temperature = &temperature
	second.Title, third.Title = "Second", "Third"
	d.Assistants = []assistants.Definition{p, second, third}
	rendered := d.Render()
	var cfg struct {
		Providers map[string]struct {
			Models map[string]struct {
				Variants []struct {
					ID   string
					Body map[string]any
				}
				ModelID string
			}
		}
	}
	if err := json.Unmarshal(rendered["opencode.jsonc"], &cfg); err != nil {
		t.Fatal(err, string(rendered["opencode.jsonc"]))
	}
	models := cfg.Providers["litellm"].Models
	if len(models["standard"].Variants) != 2 || models["standard"].Variants[0].ID != p.AgentName() || models["standard"].Variants[0].Body["temperature"] != 0.2 || models["standard"].Variants[0].Body["max_tokens"] != float64(2048) {
		t.Fatal(models)
	}
	if models["standard"].Variants[1].ID != "profile-third" || models["standard"].Variants[1].Body["temperature"] != 0.7 {
		t.Fatal("profiles sharing a role lost independent settings", models)
	}
	if len(models["fast"].Variants) != 1 || models["fast"].Variants[0].Body["reasoning_effort"] != "none" || len(models["thorough"].Variants) != 0 {
		t.Fatal("variants crossed roles", models)
	}
	if !strings.Contains(string(rendered["agents/profile-it-hilfe.md"]), "model: litellm/standard#profile-it-hilfe") {
		t.Fatal("agent did not select generated variant")
	}
	d.Artifacts["opencode.jsonc"] = strings.Replace(d.Artifacts["opencode.jsonc"], "[[profiles.default_variants]]", "", 1)
	if err := d.Validate(); err == nil {
		t.Fatal("missing variant slot accepted")
	}
	// Without tuned profiles the raw slots disappear, preserving provider defaults.
	d.Assistants = []assistants.Definition{}
	if strings.Contains(d.tokens()["profiles.small_variants"], "variants") {
		t.Fatal("empty role overrides provider variants")
	}
}
