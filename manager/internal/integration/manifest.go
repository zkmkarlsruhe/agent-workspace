// SPDX-License-Identifier: MIT

// Package integration defines the schema-1 contracts and shipped builtin inventory.
package integration

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
)

// Manifest describes one integration, independently of instance grants.
type Manifest struct {
	Schema            int             `json:"schema"`
	Name              string          `json:"name"`
	Version           string          `json:"version"`
	Runs              string          `json:"runs"`
	Kind              string          `json:"kind,omitempty"`
	RequiresCore      string          `json:"requires_core,omitempty"`
	RequiresOpenCode  string          `json:"requires_opencode,omitempty"`
	Requires          []string        `json:"requires,omitempty"`
	Conflicts         []string        `json:"conflicts,omitempty"`
	EnabledByDefault  bool            `json:"enabled_by_default,omitempty"`
	Image             string          `json:"image,omitempty"`
	Skills            string          `json:"skills,omitempty"`
	Prompt            string          `json:"prompt,omitempty"`
	MCP               *MCP            `json:"mcp,omitempty"`
	Tools             []string        `json:"tools,omitempty"`
	Events            []EventType     `json:"events,omitempty"`
	EventConsumer     string          `json:"event_consumer,omitempty"`
	Scopes            []string        `json:"scopes,omitempty"`
	Portal            []PortalPage    `json:"portal,omitempty"`
	Settings          json.RawMessage `json:"settings,omitempty"`
	PersonSettings    json.RawMessage `json:"person_settings,omitempty"`
	Consent           *Consent        `json:"consent,omitempty"`
	HoldsPersonalData bool            `json:"holds_personal_data,omitempty"`
	DeletionContract  string          `json:"deletion_contract,omitempty"`
	Health            string          `json:"health,omitempty"`
	Limits            *Limits         `json:"limits,omitempty"`
	Owner             *Owner          `json:"owner,omitempty"`
}

type PortalPage struct {
	Path  string   `json:"path"`
	Roles []string `json:"roles"`
}

type Consent struct {
	Required bool   `json:"required"`
	Version  int    `json:"version,omitempty"`
	Text     string `json:"text,omitempty"`
	TextEN   string `json:"text_en,omitempty"`
}

type Limits struct {
	TimeoutS    int     `json:"timeout_s,omitempty"`
	Concurrency int     `json:"concurrency,omitempty"`
	LLMEURMonth float64 `json:"llm_eur_month,omitempty"`
}

type Owner struct {
	Name    string `json:"name"`
	Contact string `json:"contact"`
}

// MCP is either a service-relative path or a remote URL and secret reference.
type MCP struct {
	Path       string
	URL        string
	AuthSecret string
}

func (m *MCP) UnmarshalJSON(b []byte) error {
	*m = MCP{}
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &m.Path)
	}
	var remote struct {
		URL        string `json:"url"`
		AuthSecret string `json:"auth_secret,omitempty"`
	}
	if err := strictJSON(b, &remote); err != nil {
		return err
	}
	m.URL, m.AuthSecret = remote.URL, remote.AuthSecret
	if m.URL == "" {
		return fmt.Errorf("mcp.url is required")
	}
	return nil
}

func (m MCP) MarshalJSON() ([]byte, error) {
	if m.Path != "" {
		return json.Marshal(m.Path)
	}
	return json.Marshal(struct {
		URL        string `json:"url"`
		AuthSecret string `json:"auth_secret,omitempty"`
	}{m.URL, m.AuthSecret})
}

var (
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	toolRE   = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	secretRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	imageRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}$`)
)

func validName(s string) bool { return len(s) <= 40 && nameRE.MatchString(s) && s != "base" }

// ToolName resolves the renderer reference {{tool <integration>.<tool>}}.
// Hyphens stay literal; underscores cannot occur in integration names.
func ToolName(integration, tool string) (string, error) {
	name := integration + "_" + tool
	if !validName(integration) || !toolRE.MatchString(tool) || len(name) > 64 {
		return "", fmt.Errorf("invalid tool reference %q.%q", integration, tool)
	}
	return name, nil
}

func localPath(s string) bool {
	s = strings.TrimSuffix(s, "/")
	return s != "" && s != "." && !strings.ContainsAny(s, `\:%`) && fs.ValidPath(s)
}

func route(s string) bool {
	if !strings.HasPrefix(s, "/") || s == "/" || path.Clean(s) != s || strings.ContainsAny(s, "\\%?#{}") {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func unique(values []string, valid func(string) bool) bool {
	seen := map[string]bool{}
	for _, s := range values {
		if seen[s] || !valid(s) {
			return false
		}
		seen[s] = true
	}
	return true
}

// Validate checks a single manifest. Resolve checks the selected set as a whole.
func (m Manifest) Validate() error {
	bad := func(field string) error { return fmt.Errorf("integration %q: invalid %s", m.Name, field) }
	if m.Schema != 1 {
		return bad("schema (want 1)")
	}
	if !validName(m.Name) {
		return bad("name")
	}
	if _, err := parseVersion(m.Version, false); err != nil {
		return bad("version")
	}
	if m.Runs != "none" && m.Runs != "builtin" && m.Runs != "service" {
		return bad("runs")
	}
	if m.Kind != "" && m.Kind != "builtin" && m.Kind != "remote" {
		return bad("kind (builtin or remote)")
	}
	if m.Kind == "builtin" && m.Runs != "builtin" {
		return bad("builtin kind requires runs builtin")
	}
	for _, r := range []string{m.RequiresCore, m.RequiresOpenCode} {
		if _, err := satisfies("0.0.0", r); err != nil {
			return bad("version requirement: " + r)
		}
	}
	for _, list := range [][]string{m.Requires, m.Conflicts} {
		if !unique(list, func(s string) bool { return validName(s) && s != m.Name }) {
			return bad("requires/conflicts")
		}
	}
	if m.Image != "" && (m.Runs != "service" || !imageRE.MatchString(m.Image)) {
		return bad("image (service image pinned by sha256)")
	}
	for _, p := range []string{m.Skills, m.Prompt} {
		if p != "" && !localPath(p) {
			return bad("skills/prompt path")
		}
	}
	if m.MCP != nil {
		p := m.MCP
		switch {
		case p.Path != "":
			if m.Runs != "service" || !route(p.Path) || p.URL != "" || p.AuthSecret != "" {
				return bad("mcp service path")
			}
		default:
			u, err := url.Parse(p.URL)
			if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(p.URL, "#") || u.Opaque != "" {
				return bad("mcp URL (http(s), no credentials, query or fragment)")
			}
			if p.AuthSecret != "" && !secretRE.MatchString(p.AuthSecret) {
				return bad("mcp.auth_secret reference")
			}
		}
	}
	if !unique(m.Tools, func(s string) bool {
		_, err := ToolName(m.Name, s)
		return err == nil
	}) {
		return bad("tools")
	}
	if len(m.Tools) > 0 && m.Runs != "builtin" && m.MCP == nil {
		return bad("tools without mcp")
	}
	events := make([]string, len(m.Events))
	for i, e := range m.Events {
		events[i] = string(e)
	}
	if !unique(events, func(s string) bool { return knownEvent(EventType(s)) }) {
		return bad("events")
	}
	if (len(m.Events) > 0 && m.Runs == "none") || (m.EventConsumer != "" && (!validName(m.EventConsumer) || len(m.Events) == 0)) {
		return bad("event_consumer/events")
	}
	if !unique(m.Scopes, knownScope) {
		return bad("scopes")
	}
	paths := make([]string, len(m.Portal))
	for i, p := range m.Portal {
		paths[i] = p.Path
		if len(p.Roles) == 0 || !unique(p.Roles, validName) {
			return bad("portal roles")
		}
	}
	if !unique(paths, route) || (len(m.Portal) > 0 && m.Runs == "none") {
		return bad("portal paths")
	}
	for _, s := range []json.RawMessage{m.Settings, m.PersonSettings} {
		if len(s) > 0 {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(s, &obj); err != nil || obj == nil {
				return bad("settings/person_settings schema object")
			}
		}
	}
	if c := m.Consent; c != nil {
		if c.Version < 0 || (c.Required && (c.Version < 1 || c.Text == "")) {
			return bad("consent version/text")
		}
		for _, p := range []string{c.Text, c.TextEN} {
			if p != "" && !localPath(p) {
				return bad("consent text path")
			}
		}
		if (c.Text != "" || c.TextEN != "") && c.Version < 1 {
			return bad("consent version")
		}
	}
	if m.DeletionContract != "" && m.DeletionContract != "v1" {
		return bad("deletion_contract")
	}
	if m.HoldsPersonalData && m.Runs != "builtin" && m.DeletionContract != "v1" {
		return bad("holds_personal_data without deletion_contract v1")
	}
	if m.Health != "" && m.Health != "optional" && m.Health != "critical" {
		return bad("health")
	}
	if l := m.Limits; l != nil && (l.TimeoutS < 0 || l.Concurrency < 0 || l.LLMEURMonth < 0 || math.IsNaN(l.LLMEURMonth) || math.IsInf(l.LLMEURMonth, 0)) {
		return bad("limits")
	}
	if o := m.Owner; o != nil && (strings.TrimSpace(o.Name) == "" || strings.TrimSpace(o.Contact) == "") {
		return bad("owner")
	}
	return nil
}

// BuiltinTools distinguishes manager tools from broker tools without name rules.
// Omitted kind preserves the legacy runs=builtin classification.
func (m Manifest) BuiltinTools() bool { return m.Runs == "builtin" && m.Kind != "remote" }

func knownScope(s string) bool {
	switch s {
	case "transcripts:read", "llm.background", "llm.user", "workspace:read", "workspace:write", "users:read", "notify":
		return true
	}
	return false
}
