// SPDX-License-Identifier: MIT

package integration

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

const minimalJSON = `{"schema":1,"name":"example","version":"1.2.3","runs":"builtin"}`

func manifestJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(minimalJSON), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]any
		valid  bool
	}{
		{"minimal", nil, true},
		{"builtin kind", map[string]any{"kind": "builtin"}, true},
		{"remote kind", map[string]any{"kind": "remote"}, true},
		{"invalid kind", map[string]any{"kind": "other"}, false},
		{"service builtin kind", map[string]any{"kind": "builtin", "runs": "service"}, false},
		{"none", map[string]any{"runs": "none"}, true},
		{"service", map[string]any{"runs": "service", "mcp": "/mcp"}, true},
		{"remote", map[string]any{"runs": "none", "mcp": map[string]any{"url": "https://example.org/mcp", "auth_secret": "TICKET_TOKEN"}, "tools": []string{"fetch"}}, true},
		{"internal HTTP", map[string]any{"mcp": map[string]any{"url": "http://service:8080/mcp"}}, true},
		{"digest", map[string]any{"runs": "service", "image": "registry.example.org/report@sha256:" + strings.Repeat("a", 64)}, true},
		{"files", map[string]any{"skills": "skills/", "prompt": "prompt.md"}, true},
		{"range", map[string]any{"requires_core": ">=1.4 <2", "requires_opencode": ">=2.2.0 <2.3"}, true},
		{"calver", map[string]any{"version": "2026.10.27.1"}, true},
		{"consent", map[string]any{"consent": map[string]any{"required": true, "version": 1, "text": "consent.de.md", "text_en": "consent.en.md"}}, true},
		{"optional consent", map[string]any{"consent": map[string]any{"required": false}}, true},
		{"data builtin", map[string]any{"holds_personal_data": true}, true},
		{"data remote contract", map[string]any{"runs": "none", "holds_personal_data": true, "deletion_contract": "v1"}, true},
		{"data service contract", map[string]any{"runs": "service", "holds_personal_data": true, "deletion_contract": "v1"}, true},
		{"settings", map[string]any{"settings": map[string]any{"type": "object", "properties": map[string]any{"UPPER": map[string]any{"default": nil}}}, "person_settings": map[string]any{}}, true},
		{"events", map[string]any{"events": []string{"chat.finished", "consent.changed", "account.deleting", "usage.recorded"}, "event_consumer": "report"}, true},
		{"portal", map[string]any{"portal": []any{map[string]any{"path": "/report", "roles": []string{"admin"}}}}, true},
		{"limits", map[string]any{"limits": map[string]any{"timeout_s": 30, "concurrency": 4, "llm_eur_month": 20.5}, "health": "critical", "owner": map[string]any{"name": "Operations", "contact": "ops@example.org"}}, true},
		{"schema future", map[string]any{"schema": 2}, false},
		{"schema string", map[string]any{"schema": "1"}, false},
		{"schema null", map[string]any{"schema": nil}, false},
		{"unknown", map[string]any{"typo": true}, false},
		{"wrong case", map[string]any{"Name": "shadow"}, false},
		{"empty name", map[string]any{"name": ""}, false},
		{"reserved scope", map[string]any{"name": "base"}, false},
		{"name underscore", map[string]any{"name": "two_words"}, false},
		{"name uppercase", map[string]any{"name": "Example"}, false},
		{"name trailing hyphen", map[string]any{"name": "example-"}, false},
		{"name length", map[string]any{"name": strings.Repeat("a", 41)}, false},
		{"version incomplete", map[string]any{"version": "1.2"}, false},
		{"version prerelease", map[string]any{"version": "1.2.3-beta"}, false},
		{"version leading zero", map[string]any{"version": "01.2.3"}, false},
		{"version range typo", map[string]any{"requires_core": "^1.2"}, false},
		{"opencode range typo", map[string]any{"requires_opencode": ">=oops"}, false},
		{"runs unknown", map[string]any{"runs": "plugin"}, false},
		{"default wrong type", map[string]any{"enabled_by_default": "true"}, false},
		{"dependency duplicate", map[string]any{"requires": []string{"other", "other"}}, false},
		{"dependency self", map[string]any{"requires": []string{"example"}}, false},
		{"dependency invalid", map[string]any{"requires": []string{"Bad"}}, false},
		{"conflict self", map[string]any{"conflicts": []string{"example"}}, false},
		{"conflict duplicate", map[string]any{"conflicts": []string{"other", "other"}}, false},
		{"image tag", map[string]any{"runs": "service", "image": "registry.example.org/report:latest"}, false},
		{"image builtin", map[string]any{"image": "registry.example.org/report@sha256:" + strings.Repeat("a", 64)}, false},
		{"image bad digest", map[string]any{"runs": "service", "image": "registry.example.org/report@sha256:xyz"}, false},
		{"absolute skills", map[string]any{"skills": "/skills"}, false},
		{"parent prompt", map[string]any{"prompt": "../prompt.md"}, false},
		{"unclean prompt", map[string]any{"prompt": "a/../prompt.md"}, false},
		{"backslash prompt", map[string]any{"prompt": `a\prompt.md`}, false},
		{"encoded prompt", map[string]any{"prompt": "%2e%2e/prompt.md"}, false},
		{"root prompt", map[string]any{"prompt": "."}, false},
		{"root skills", map[string]any{"skills": "./"}, false},
		{"mcp scalar", map[string]any{"mcp": 42}, false},
		{"mcp null", map[string]any{"mcp": nil}, false},
		{"mcp empty", map[string]any{"mcp": map[string]any{}}, false},
		{"mcp unknown field", map[string]any{"mcp": map[string]any{"url": "https://example.org", "token": "secret"}}, false},
		{"mcp wrong case", map[string]any{"mcp": map[string]any{"URL": "https://example.org"}}, false},
		{"mcp credential", map[string]any{"mcp": map[string]any{"url": "https://user:password@example.org/mcp"}}, false},
		{"mcp query", map[string]any{"mcp": map[string]any{"url": "https://example.org/mcp?token=value"}}, false},
		{"mcp fragment", map[string]any{"mcp": map[string]any{"url": "https://example.org/mcp#token"}}, false},
		{"mcp empty query", map[string]any{"mcp": map[string]any{"url": "https://example.org/mcp?"}}, false},
		{"mcp empty fragment", map[string]any{"mcp": map[string]any{"url": "https://example.org/mcp#"}}, false},
		{"mcp scheme", map[string]any{"mcp": map[string]any{"url": "file:///mcp"}}, false},
		{"mcp no host", map[string]any{"mcp": map[string]any{"url": "https:///mcp"}}, false},
		{"mcp secret value", map[string]any{"mcp": map[string]any{"url": "https://example.org/mcp", "auth_secret": "Bearer secret"}}, false},
		{"mcp relative path", map[string]any{"runs": "service", "mcp": "mcp"}, false},
		{"mcp builtin path", map[string]any{"mcp": "/mcp"}, false},
		{"tools duplicate", map[string]any{"tools": []string{"fetch", "fetch"}}, false},
		{"tools invalid", map[string]any{"tools": []string{"Fetch"}}, false},
		{"tools no transport", map[string]any{"runs": "none", "tools": []string{"fetch"}}, false},
		{"events unknown", map[string]any{"events": []string{"session.compacted"}}, false},
		{"events duplicate", map[string]any{"events": []string{"chat.finished", "chat.finished"}}, false},
		{"events no process", map[string]any{"runs": "none", "events": []string{"chat.finished"}}, false},
		{"consumer without events", map[string]any{"event_consumer": "report"}, false},
		{"consumer invalid", map[string]any{"events": []string{"chat.finished"}, "event_consumer": "Report"}, false},
		{"scope unknown", map[string]any{"scopes": []string{"users:email"}}, false},
		{"scope duplicate", map[string]any{"scopes": []string{"notify", "notify"}}, false},
		{"scope null element", map[string]any{"scopes": []any{nil}}, false},
		{"scope null list", map[string]any{"scopes": nil}, false},
		{"portal no roles", map[string]any{"portal": []any{map[string]any{"path": "/report"}}}, false},
		{"portal duplicate roles", map[string]any{"portal": []any{map[string]any{"path": "/report", "roles": []string{"admin", "admin"}}}}, false},
		{"portal none", map[string]any{"runs": "none", "portal": []any{map[string]any{"path": "/report", "roles": []string{"admin"}}}}, false},
		{"settings scalar", map[string]any{"settings": "value"}, false},
		{"person settings array", map[string]any{"person_settings": []string{}}, false},
		{"settings null", map[string]any{"settings": nil}, false},
		{"consent version missing", map[string]any{"consent": map[string]any{"required": true, "text": "consent.md"}}, false},
		{"consent text missing", map[string]any{"consent": map[string]any{"required": true, "version": 1}}, false},
		{"consent path", map[string]any{"consent": map[string]any{"required": true, "version": 1, "text": "../consent.md"}}, false},
		{"consent translation path", map[string]any{"consent": map[string]any{"version": 1, "text_en": "/consent.md"}}, false},
		{"consent optional text unversioned", map[string]any{"consent": map[string]any{"text": "consent.md"}}, false},
		{"consent unknown", map[string]any{"consent": map[string]any{"required": false, "typo": 1}}, false},
		{"consent negative version", map[string]any{"consent": map[string]any{"version": -1}}, false},
		{"remote data no contract", map[string]any{"runs": "none", "holds_personal_data": true}, false},
		{"service data no contract", map[string]any{"runs": "service", "holds_personal_data": true}, false},
		{"deletion future", map[string]any{"deletion_contract": "v2"}, false},
		{"health unknown", map[string]any{"health": "healthy"}, false},
		{"timeout negative", map[string]any{"limits": map[string]any{"timeout_s": -1}}, false},
		{"concurrency negative", map[string]any{"limits": map[string]any{"concurrency": -1}}, false},
		{"budget negative", map[string]any{"limits": map[string]any{"llm_eur_month": -0.1}}, false},
		{"limits unknown", map[string]any{"limits": map[string]any{"timeout": 10}}, false},
		{"owner incomplete", map[string]any{"owner": map[string]any{"name": "Ops"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Load(strings.NewReader(manifestJSON(t, tt.fields)))
			if (err == nil) != tt.valid {
				t.Fatalf("Load error = %v; valid = %v", err, tt.valid)
			}
			if err == nil {
				b, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Load(strings.NewReader(string(b))); err != nil {
					t.Fatalf("round trip: %v", err)
				}
			}
		})
	}
}

func TestLoadMalformed(t *testing.T) {
	for _, raw := range []string{
		"", "null", "[]", "true", "{", minimalJSON + "{}", minimalJSON + "junk",
		`{"schema":1,"name":"example","name":"shadow","version":"1.2.3","runs":"builtin"}`,
		strings.Replace(minimalJSON, `"schema":1`, `"schema":1.0`, 1),
		strings.Replace(minimalJSON, `"schema":1,`, "", 1),
		strings.Replace(minimalJSON, `"name":"example",`, "", 1),
		strings.Replace(minimalJSON, `"version":"1.2.3",`, "", 1),
		strings.Replace(minimalJSON, `,"runs":"builtin"`, "", 1),
		strings.TrimSuffix(minimalJSON, "}") + `,"settings":{"type":"object","type":"string"}}`,
		strings.TrimSuffix(minimalJSON, "}") + `,"consent":{"required":false,"required":true}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Load(strings.NewReader(raw)); err == nil {
				t.Fatal("accepted malformed manifest")
			}
		})
	}
}

func TestPortalPaths(t *testing.T) {
	for _, p := range []string{"report", "/", "/a/../report", "/report/", "/report?x=1", "/report#x", "/%72eport", "/{name}", "/two words", ` /report`, `/a\b`, "/null\x00", "/vertical\v", "/space\u00a0"} {
		t.Run(p, func(t *testing.T) {
			raw := manifestJSON(t, map[string]any{"portal": []any{map[string]any{"path": p, "roles": []string{"admin"}}}})
			if _, err := Load(strings.NewReader(raw)); err == nil {
				t.Fatal("accepted invalid portal path")
			}
		})
	}
	page := map[string]any{"path": "/report", "roles": []string{"admin"}}
	if _, err := Load(strings.NewReader(manifestJSON(t, map[string]any{"portal": []any{page, page}}))); err == nil {
		t.Fatal("accepted duplicate portal path")
	}
}

func TestLoadYAML(t *testing.T) {
	base := "schema: 1\nname: example\nversion: 1.2.3\nruns: builtin\n"
	for _, tt := range []struct {
		name, raw string
		valid     bool
	}{
		{"minimal", base, true},
		{"JSON", minimalJSON, true},
		{"remote", base + "mcp: {url: 'https://example.org/mcp', auth_secret: TOKEN}\n", true},
		{"service", strings.Replace(base, "builtin", "service", 1) + "mcp: /mcp\n", true},
		{"range", base + "requires_opencode: '>=2.2.0 <2.3'\n", true},
		{"integer schema only", strings.Replace(base, "schema: 1", "schema: 1.0", 1), false},
		{"float budget", base + "limits: {llm_eur_month: 20.0}\n", true},
		{"fractional budget", base + "limits: {llm_eur_month: .5}\n", true},
		{"nonfinite budget", base + "limits: {llm_eur_month: .inf}\n", false},
		{"timestamp tag", base + "prompt: 2026-10-27\n", false},
		{"nested duplicate", base + "consent: {required: false, required: true}\n", false},
		{"empty", "", false},
		{"malformed", "[", false},
		{"duplicate", base + "name: shadow\n", false},
		{"unknown", base + "unknown: true\n", false},
		{"nested unknown", base + "consent: {required: false, unknown: true}\n", false},
		{"null", base + "consent: null\n", false},
		{"number as string", strings.Replace(base, "name: example", "name: 42", 1), false},
		{"tag", base + "prompt: !custom file.md\n", false},
		{"alias", base + "prompt: &p file.md\nskills: *p\n", false},
		{"merge", base + "<<: {name: shadow}\n", false},
		{"non string key", base + "settings: {1: value}\n", false},
		{"extra document", base + "---\n" + base, false},
		{"empty extra document", base + "---\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LoadYAML(strings.NewReader(tt.raw)); (err == nil) != tt.valid {
				t.Fatalf("LoadYAML error = %v; valid = %v", err, tt.valid)
			}
		})
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestLoadReadError(t *testing.T) {
	if _, err := Load(brokenReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error = %v", err)
	}
}

func TestToolName(t *testing.T) {
	for _, tt := range []struct {
		integration, tool, want string
	}{
		{"pdf", "md_to_pdf", "pdf_md_to_pdf"},
		{"nextcloud", "fetch", "nextcloud_fetch"},
		{"web-research", "web_search", "web-research_web_search"},
		{"a", strings.Repeat("b", 62), "a_" + strings.Repeat("b", 62)},
		{"a", strings.Repeat("b", 63), ""},
		{"base", "fetch", ""},
		{"foo_bar", "fetch", ""},
		{"", "fetch", ""},
		{"pdf", "", ""},
		{"pdf", "MD", ""},
		{"pdf", "md-to-pdf", ""},
		{"pdf", "md__to_pdf", ""},
		{"pdf", "md_to_pdf_", ""},
		{"pdf", "../fetch", ""},
	} {
		t.Run(tt.integration+"."+tt.tool, func(t *testing.T) {
			got, err := ToolName(tt.integration, tt.tool)
			if got != tt.want || (err == nil) != (tt.want != "") {
				t.Fatalf("ToolName = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestValidateConstructedValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*Manifest)
	}{
		{"nan budget", func(m *Manifest) { m.Limits = &Limits{LLMEURMonth: math.NaN()} }},
		{"infinite budget", func(m *Manifest) { m.Limits = &Limits{LLMEURMonth: math.Inf(1)} }},
		{"ambiguous mcp", func(m *Manifest) {
			m.Runs = "service"
			m.MCP = &MCP{Path: "/mcp", URL: "https://example.org"}
		}},
		{"path with secret", func(m *Manifest) {
			m.Runs = "service"
			m.MCP = &MCP{Path: "/mcp", AuthSecret: "TOKEN"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := builtin("example")
			tt.edit(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("accepted invalid programmatically constructed manifest")
			}
		})
	}
}
