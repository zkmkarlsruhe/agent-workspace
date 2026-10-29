// SPDX-License-Identifier: MIT

// Package instance loads the installation's non-secret values and renders its assets.
package instance

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	_ "time/tzdata" // The scratch image has no system timezone database.

	"go.yaml.in/yaml/v3"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/integration"
)

type Folders struct {
	Inbox       string `yaml:"inbox"`
	Results     string `yaml:"results"`
	Work        string `yaml:"work"`
	ProjectMeta string `yaml:"project_meta"`
	Brief       string `yaml:"brief"`
	Trash       string `yaml:"trash"`
}

type Models struct {
	Default  string `yaml:"default"`
	Small    string `yaml:"small"`
	Thorough string `yaml:"thorough"`
}

type Notice struct {
	Title  string   `yaml:"title"`
	Points []string `yaml:"points"`
	Short  string   `yaml:"short"`
}

type Consent struct {
	Version string `yaml:"version"`
	EN      Notice `yaml:"en"`
	DE      Notice `yaml:"de"`
}

type Localized struct {
	EN string `yaml:"en"`
	DE string `yaml:"de"`
}

// FootprintModel describes one configured role; aliases come from Models.
type FootprintModel struct {
	Name     string  `yaml:"name"`
	Provider string  `yaml:"provider"`
	EOut     float64 `yaml:"e_out"`
	EIn      float64 `yaml:"e_in"`
	LoF      float64 `yaml:"lo_factor"`
	HiF      float64 `yaml:"hi_factor"`
	Rough    bool    `yaml:"rough"`
}

// FootprintAssumptions are explicit estimates, not measurements of the active provider.
type FootprintAssumptions struct {
	PUE         float64 `yaml:"pue"`
	GridGPerKWh float64 `yaml:"grid_g_per_kwh"`
}

type Sessions struct {
	ArchiveAfterDays float64 `yaml:"archive_after_days"`
}

type Definition struct {
	Timezone             string `yaml:"timezone"`
	location             *time.Location
	FootprintAssumptions FootprintAssumptions `yaml:"footprint_assumptions"`
	Default              bool                 `yaml:"default,omitempty"`
	Accounting           Accounting           `yaml:"accounting"`
	tokenValues          map[string]string
	Sessions             Sessions     `yaml:"sessions"`
	Capabilities         Capabilities `yaml:"capabilities"`
	// LegacyProfiles is read for one release; assistants takes precedence.
	LegacyProfiles    []assistants.Definition `yaml:"profiles,omitempty"`
	Integrations      map[string]bool         `yaml:"integrations"`
	IntegrationGrants map[string][]string     `yaml:"integration_grants"`
	MCPServer         string                  `yaml:"mcp_server"`
	resolved          []integration.Manifest
	manifests         []integration.Manifest
	Catalog           []CatalogEntry `yaml:"catalog"`

	Assistants       []assistants.Definition   `yaml:"assistants"`
	Schema           int                       `yaml:"schema"`
	Values           map[string]string         `yaml:"values"`
	Folders          Folders                   `yaml:"folders"`
	Models           Models                    `yaml:"models"`
	Defaults         map[string]string         `yaml:"defaults"`
	Consent          Consent                   `yaml:"consent"`
	Artifacts        map[string]string         `yaml:"artifacts"`
	Copy             map[string]Localized      `yaml:"copy"`
	ReferenceHosts   []string                  `yaml:"reference_hosts"`
	Footprint        map[string]FootprintModel `yaml:"footprint"`
	KnowledgeSources assistants.Sources        `yaml:"knowledge_sources"`
}

//go:embed *.yaml
var embedded embed.FS

var selected atomic.Pointer[Definition]

func init() {
	d, err := loadDefault(embedded)
	if errors.Is(err, errSelectionRequired) {
		// Unselected public builds need neutral values during package initialization.
		var b []byte
		b, err = embedded.ReadFile("minimal.yaml")
		if err == nil {
			d, err = Parse(b)
		}
	}
	if err != nil {
		panic(err)
	}
	Select(d)
}

// Current is immutable after selection. One manager process serves one instance.
func Current() *Definition { return selected.Load() }

// Location is the validated timezone of the selected instance.
func Location() *time.Location { return Current().location }

// Select is called before starting services; the returned restore is for serial tests.
// shortcut: live instance switching is unsupported; restart after changing the file.
func Select(d *Definition) func() {
	d.tokenValues = d.buildTokens()
	old := selected.Swap(d)
	// Keep local timestamps in transcripts and exports on the instance clock.
	previousLocation := time.Local
	time.Local = d.location
	return func() { selected.Store(old); time.Local = previousLocation }
}

// Load uses the flagged embedded default, or minimal when it is the only definition.
func Load(filename string) (*Definition, error) {
	return LoadWithIntegrations(filename, strings.TrimSpace(os.Getenv("OC_INTEGRATIONS_DIR")))
}

// LoadWithIntegrations snapshots the operator directory before validating the instance.
func LoadWithIntegrations(filename, dir string) (*Definition, error) {
	manifests, err := integration.Inventory(dir)
	if err != nil {
		return nil, err
	}
	if filename == "" {
		return loadDefaultWith(embedded, manifests)
	}
	b, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("instance: %w", err)
	}
	return parse(b, manifests)
}

var errSelectionRequired = errors.New("instance: set OC_INSTANCE_FILE to an instance.yaml path (renderer: -instance); without a flagged default, automatic selection requires minimal to be the only embedded instance")

func loadDefault(files fs.FS) (*Definition, error) {
	return loadDefaultWith(files, nil)
}

func loadDefaultWith(files fs.FS, manifests []integration.Manifest) (*Definition, error) {
	names, err := fs.Glob(files, "*.yaml")
	if err != nil {
		return nil, fmt.Errorf("instance: %w", err)
	}
	var chosen, minimal *Definition
	for _, name := range names {
		b, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, fmt.Errorf("instance: %w", err)
		}
		d, err := parse(b, manifests)
		if err != nil {
			return nil, fmt.Errorf("embedded %s: %w", name, err)
		}
		if name == "minimal.yaml" {
			minimal = d
		}
		if d.Default {
			if chosen != nil {
				return nil, errors.New("instance: multiple embedded definitions have default: true")
			}
			chosen = d
		}
	}
	if chosen != nil {
		return chosen, nil
	}
	if len(names) == 1 && minimal != nil {
		return minimal, nil
	}
	return nil, errSelectionRequired
}

func Parse(b []byte) (*Definition, error) { return parse(b, nil) }

func parse(b []byte, manifests []integration.Manifest) (*Definition, error) {
	d := &Definition{Timezone: "UTC", manifests: manifests, FootprintAssumptions: FootprintAssumptions{PUE: 1.2, GridGPerKWh: 344}, Accounting: DefaultAccounting(), Capabilities: Capabilities{Projects: true, ProjectDefault: CapabilityDefault{Mode: "all"}, LegacyDefault: "global"}, Folders: Folders{
		Inbox: "inbox", Results: "results", Work: "work",
		ProjectMeta: ".project", Brief: "brief.md", Trash: ".trash",
	}}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(d); err != nil {
		return nil, fmt.Errorf("instance: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("instance: expected exactly one YAML document")
	}
	if d.Accounting.Prices == nil {
		d.Accounting.Prices = map[string]TokenPrice{"standard": {Input: 0.60, Output: 3.60}, "fast": {Input: 0.60, Output: 3.60}, "thorough": {Input: 0.60, Output: 3.60}}
	}
	if d.LegacyProfiles != nil {
		slog.Warn("instance: profiles is deprecated; use assistants (legacy key removed after one release)")
		if d.Assistants == nil {
			d.Assistants = d.LegacyProfiles
		}
		d.LegacyProfiles = nil
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

var component = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
var token = regexp.MustCompile(`\[\[([^\[\]\n]+)\]\]`)
var hostName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
var brandID = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
var assetPath = regexp.MustCompile(`^/opt/[a-z0-9][a-z0-9/._-]*$`)

// OverrideKeys lists the existing env defaults accepted for the transition release.
var OverrideKeys = []string{
	"OC_IDENTITY_HEADER_UID", "OC_IDENTITY_HEADER_USERNAME", "OC_IDENTITY_HEADER_EMAIL", "OC_IDENTITY_HEADER_NAME", "OC_IDENTITY_HEADER_GROUPS",
	"OC_IDENTITY_GROUPS_SEPARATOR", "OC_LOGOUT_URL",
	"OC_DOMAIN", "OC_REQUIRED_GROUP", "OC_ADMIN_GROUP", "LITELLM_BASE_URL", "OC_SEARXNG_URL",
	"OC_LDAP_URL", "OC_LDAP_BASE_DN", "OC_LDAP_BIND_DN", "OC_IDENTITY_ISSUER", "OC_IDENTITY_SUBJECT_HEADER",
	"OC_VISION_MODEL", "OC_IMAGE_MODELS", "OC_IMAGE_EDIT_MODEL", "OC_NEXTCLOUD_URL", "OC_ROOMS_URL", "OC_BIKES_URL", "OC_PDF_URL",
}

func (d *Definition) Validate() error {
	if d.Timezone == "" || d.Timezone == "Local" {
		return errors.New("instance: timezone must be an explicit IANA name")
	}
	location, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return fmt.Errorf("instance: invalid timezone %q: %w", d.Timezone, err)
	}
	d.location = location
	for _, value := range []float64{d.FootprintAssumptions.PUE, d.FootprintAssumptions.GridGPerKWh} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("instance: footprint assumptions must be finite non-negative numbers")
		}
	}
	if d.FootprintAssumptions.PUE < 1 {
		return errors.New("instance: footprint_assumptions.pue must be at least 1")
	}

	if err := d.Accounting.Validate(); err != nil {
		return err
	}
	if days := d.Sessions.ArchiveAfterDays; math.IsNaN(days) || days < 0 || days > 106751 || (days > 0 && days < 1.0/86400) {
		return errors.New("instance: sessions.archive_after_days must be 0 (off) or between 1/86400 and 106751 days")
	}
	if err := d.Capabilities.Validate(); err != nil {
		return err
	}
	if d.manifests == nil {
		var err error
		d.manifests, err = integration.Builtins()
		if err != nil {
			return err
		}
	}
	resolved, err := integration.Resolve(d.manifests, integration.Options{OpenCodeVersion: "2.2.0", Enabled: d.Integrations, Grants: d.IntegrationGrants})
	if err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	if d.MCPServer == "" {
		d.MCPServer = "workspace"
	}
	if !brandID.MatchString(d.MCPServer) {
		return errors.New("instance: invalid mcp_server")
	}
	d.resolved = resolved

	if err := d.KnowledgeSources.Validate(); err != nil {
		return err
	}
	seenAssistants := map[string]bool{}
	sharedFolders := map[string]bool{}
	for _, p := range d.Assistants {
		if err := p.Validate(); err != nil {
			return err
		}
		if _, err := p.Sources(d.KnowledgeSources); err != nil {
			return err
		}
		if seenAssistants[p.ID] {
			return fmt.Errorf("instance: duplicate assistant %s", p.ID)
		}
		seenAssistants[p.ID] = true
		for _, tool := range p.MCPTools {
			if p.Enabled && !strings.HasPrefix(tool, d.MCPServer+".") {
				return fmt.Errorf("instance: assistant %s must use organisation MCP server %s", p.ID, d.MCPServer)
			}
		}
		if p.ProjectMode == "shared" {
			name := p.SharedFolder()
			if sharedFolders[name] || slices.Contains([]string{"papierkorb", "trash", "inbox", "results", "work", "neu", "new", d.Folders.Inbox, d.Folders.Results, d.Folders.Work}, name) {
				return fmt.Errorf("instance: assistant %s has a reserved or duplicate shared folder %s", p.ID, name)
			}
			sharedFolders[name] = true
		}
	}
	if d.Schema != 1 {
		return fmt.Errorf("instance: unsupported schema %d", d.Schema)
	}
	required := "product name masthead masthead_sub organization team reviewer recipient support_url support_label website api_host domain export_prefix profile_marker brand_id config_file catalog_mount asset_root search_user_agent central_client"
	if d.Enabled("nextcloud") {
		required += " nextcloud_host nextcloud_root"
	}
	if d.Enabled("workspace-rooms") {
		required += " rooms_host bikes_host room_team city"
	}
	if d.Defaults["OC_LDAP_URL"] != "" {
		required += " directory_url"
	}
	for _, key := range strings.Fields(required) {
		v, ok := d.Values[key]
		if !ok || strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n\x00") || strings.Contains(v, "[[") {
			return fmt.Errorf("instance: missing or invalid value %s", key)
		}
	}
	for _, key := range strings.Fields("directory_url nextcloud_host nextcloud_root rooms_host bikes_host room_team city") {
		if v, ok := d.Values[key]; ok && (strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n\x00") || strings.Contains(v, "[[")) {
			return fmt.Errorf("instance: invalid value %s", key)
		}
	}
	for _, key := range []string{"export_prefix", "profile_marker", "brand_id", "config_file"} {
		if !component.MatchString(d.Values[key]) {
			return fmt.Errorf("instance: unsafe %s", key)
		}
	}
	if !brandID.MatchString(d.Values["brand_id"]) {
		return errors.New("instance: brand_id must be a CSS identifier")
	}
	for _, key := range []string{"support_url", "directory_url"} {
		if d.Values[key] == "" {
			continue
		}
		u, err := url.Parse(d.Values[key])
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return fmt.Errorf("instance: invalid %s", key)
		}
	}
	for _, key := range []string{"catalog_mount", "asset_root"} {
		v := d.Values[key]
		if !assetPath.MatchString(v) || path.Clean(v) != v {
			return fmt.Errorf("instance: unsafe %s", key)
		}
	}
	root := d.Values["nextcloud_root"]
	if strings.ContainsAny(root, "/\\") || root == "." || root == ".." || strings.TrimSpace(root) != root {
		return errors.New("instance: nextcloud_root must be one folder name")
	}
	for _, key := range []string{"domain", "website", "api_host", "nextcloud_host", "rooms_host", "bikes_host"} {
		if d.Values[key] == "" {
			continue
		}
		if !hostName.MatchString(d.Values[key]) || strings.Contains(d.Values[key], "..") {
			return fmt.Errorf("instance: invalid host %s", key)
		}
	}
	for _, host := range d.ReferenceHosts {
		if !hostName.MatchString(host) || !strings.Contains(host, ".") || strings.Contains(host, "..") {
			return fmt.Errorf("instance: invalid reference host %q", host)
		}
	}
	seen := map[string]bool{}
	for _, f := range []string{d.Folders.Inbox, d.Folders.Results, d.Folders.Work, d.Folders.ProjectMeta, d.Folders.Trash} {
		if !component.MatchString(strings.TrimPrefix(f, ".")) || seen[f] || f == ".git" || f == ".opencode" {
			return fmt.Errorf("instance: unsafe or duplicate folder %q", f)
		}
		seen[f] = true
	}
	if strings.HasPrefix(d.Folders.Inbox, ".") || strings.HasPrefix(d.Folders.Results, ".") || strings.HasPrefix(d.Folders.Work, ".") ||
		!strings.HasPrefix(d.Folders.ProjectMeta, ".") || !strings.HasPrefix(d.Folders.Trash, ".") || !component.MatchString(d.Folders.Brief) {
		return errors.New("instance: input/results/work must be visible, metadata/trash hidden, brief a basename")
	}
	for _, model := range []string{d.Models.Default, d.Models.Small, d.Models.Thorough} {
		if model == "" || strings.ContainsAny(model, "\r\n\x00") || strings.Contains(model, "[[") {
			return errors.New("instance: default, small and thorough models are required")
		}
	}
	for role, model := range d.Footprint {
		if role != "default" && role != "small" && role != "thorough" {
			return fmt.Errorf("instance: unknown footprint role %s", role)
		}
		if model.Name == "" || model.Provider == "" || model.LoF <= 0 || model.HiF < model.LoF {
			return fmt.Errorf("instance: invalid footprint model %s", role)
		}
		for _, value := range []float64{model.EOut, model.EIn, model.LoF, model.HiF} {
			if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("instance: invalid footprint coefficient %s", role)
			}
		}
	}
	allowed := map[string]bool{}
	for _, k := range OverrideKeys {
		allowed[k] = true
	}
	for k := range d.Defaults {
		if !allowed[k] {
			return fmt.Errorf("instance: unsupported default %s (secrets stay in env)", k)
		}
	}
	for _, key := range []string{"OC_DOMAIN", "OC_IDENTITY_ISSUER", "OC_IDENTITY_SUBJECT_HEADER"} {
		if strings.TrimSpace(d.Defaults[key]) == "" {
			return fmt.Errorf("instance: missing default %s", key)
		}
	}
	for _, key := range []string{"LITELLM_BASE_URL", "OC_SEARXNG_URL", "OC_PDF_URL", "OC_NEXTCLOUD_URL", "OC_ROOMS_URL", "OC_BIKES_URL"} {
		if raw := d.Defaults[key]; raw != "" {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("instance: invalid endpoint %s", key)
			}
		}
	}
	if d.Consent.Version == "" {
		return errors.New("instance: consent version is required")
	}
	for _, n := range []Notice{d.Consent.EN, d.Consent.DE} {
		if n.Title == "" || n.Short == "" || len(n.Points) == 0 {
			return errors.New("instance: English and German consent are required")
		}
		for _, s := range append([]string{n.Title, n.Short}, n.Points...) {
			if err := d.checkTokens(s); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"evaluation_help", "evaluation_privacy", "providers", "folder_name_prompt"} {
		c := d.Copy[key]
		if c.EN == "" || c.DE == "" {
			return fmt.Errorf("instance: missing localized copy %s", key)
		}
	}
	for key, c := range d.Copy {
		if c.EN == "" || c.DE == "" {
			return fmt.Errorf("instance: missing localized copy %s", key)
		}
		for _, text := range []string{c.EN, c.DE} {
			if strings.Contains(text, "[[copy.") {
				return errors.New("instance: copy cannot reference other copy fields")
			}
			if err := d.checkTokens(text); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"opencode.jsonc", "AGENTS.md", "agents/helper.md"} {
		if d.Artifacts[name] == "" {
			return fmt.Errorf("instance: missing artifact %s", name)
		}
	}
	for _, p := range d.Assistants {
		if len(p.ModelBody()) > 0 && strings.Count(d.Artifacts["opencode.jsonc"], "[[assistants."+p.ModelRole+"_variants]]")+strings.Count(d.Artifacts["opencode.jsonc"], "[[profiles."+p.ModelRole+"_variants]]") != 1 {
			return fmt.Errorf("instance: assistant %s requires one [[assistants.%s_variants]] token in opencode.jsonc", p.ID, p.ModelRole)
		}
	}
	for name, s := range d.Artifacts {
		if name != "opencode.jsonc" && name != "AGENTS.md" && name != "agents/helper.md" && name != "commands/help.md" {
			return fmt.Errorf("instance: unsupported artifact %s", name)
		}
		if err := d.checkTokens(s); err != nil {
			return err
		}
	}
	return nil
}

func (d *Definition) tokens() map[string]string {
	if d.tokenValues != nil {
		return d.tokenValues
	}
	return d.buildTokens()
}

func (d *Definition) buildTokens() map[string]string {
	m := make(map[string]string, len(d.Values)+9)
	for k, v := range d.Values {
		m[k] = v
	}
	m["mcp_server"] = d.MCPServer
	m["timezone"] = d.Timezone
	m["folders.inbox"], m["folders.results"], m["folders.work"] = d.Folders.Inbox, d.Folders.Results, d.Folders.Work
	m["folders.project_meta"], m["folders.brief"], m["folders.trash"] = d.Folders.ProjectMeta, d.Folders.Brief, d.Folders.Trash
	m["models.default"], m["models.small"], m["models.thorough"] = d.Models.Default, d.Models.Small, d.Models.Thorough
	for _, role := range []string{"default", "small", "thorough"} {
		variants := []map[string]any{}
		for _, p := range d.Assistants {
			if body := p.ModelBody(); p.ModelRole == role && len(body) > 0 {
				variants = append(variants, map[string]any{"id": p.AgentName(), "body": body})
			}
		}
		b, _ := json.Marshal(variants)
		m["assistants."+role+"_variants"] = ""
		if len(variants) > 0 {
			m["assistants."+role+"_variants"] = `"variants": ` + string(b) + ","
		}
		m["profiles."+role+"_variants"] = m["assistants."+role+"_variants"]
	}
	// Runtime model prices are USD; omit them without an explicit rate or role price.
	m["accounting.upstream_cost_currency"] = d.Accounting.UpstreamCostCurrency
	m["accounting.usd_per_eur"] = strconv.FormatFloat(d.Accounting.USDPerEUR, 'f', -1, 64)
	var prices []string
	for _, role := range []string{"standard", "fast", "thorough"} {
		key := "accounting." + role + "_cost"
		m[key] = ""
		p, ok := d.Accounting.Prices[role]
		if !ok {
			continue
		}
		prices = append(prices, fmt.Sprintf("%s %g/%g", role, p.Input, p.Output))
		if d.Accounting.USDPerEUR > 0 {
			// Fifteen significant digits suppress binary multiplication noise in display prices.
			m[key] = fmt.Sprintf(`, "cost": { "input": %.15g, "output": %.15g }`, p.Input*d.Accounting.USDPerEUR, p.Output*d.Accounting.USDPerEUR)
		}
	}
	m["accounting.prices"] = strings.Join(prices, "; ")
	if len(prices) == 0 {
		m["accounting.prices"] = "—"
	}
	// Copy uses the same value tokens as templates; nested copy references are rejected.
	for key, c := range d.Copy {
		for lang, value := range map[string]string{"en": c.EN, "de": c.DE} {
			m["copy."+key+"."+lang] = token.ReplaceAllStringFunc(value, func(t string) string {
				name := t[2 : len(t)-2]
				if strings.HasPrefix(name, "copy.") {
					return t
				}
				if v, ok := m[name]; ok {
					return v
				}
				return t
			})
		}
	}
	return m
}

func (d *Definition) checkTokens(s string) error {
	var err error
	s, err = d.conditional(s)
	if err != nil {
		return err
	}
	values := d.tokens()
	for _, match := range token.FindAllStringSubmatch(s, -1) {
		if _, ok := values[match[1]]; !ok {
			return fmt.Errorf("instance: unknown token %s", match[1])
		}
	}
	return nil
}

// Text renders explicit template tokens, never institution names or arbitrary output.
// Markup belongs to the trusted template; inserted instance values are escaped separately.
func (d *Definition) Text(s string, markup, format bool) string {
	values := d.tokens()
	return token.ReplaceAllStringFunc(s, func(t string) string {
		v, ok := values[t[2:len(t)-2]]
		if !ok {
			return t
		}
		if markup {
			v = html.EscapeString(v)
		}
		if format {
			v = strings.ReplaceAll(v, "%", "%%")
		}
		return v
	})
}

func (d *Definition) Notice(lang string) Notice {
	n := d.Consent.DE
	if lang == "en" {
		n = d.Consent.EN
	}
	n.Title, n.Short = d.Text(n.Title, false, false), d.Text(n.Short, false, false)
	points := make([]string, len(n.Points))
	for i, s := range n.Points {
		points[i] = d.Text(s, false, false)
	}
	n.Points = points
	return n
}

// Render returns deterministic assets; JSON string tokens are escaped for JSONC.
func (d *Definition) Render() map[string][]byte {
	out := make(map[string][]byte, len(d.Artifacts)+2)
	for name, s := range d.Artifacts {
		s, _ = d.conditional(s) // Validate rejected malformed conditions.
		if name == "opencode.jsonc" {
			values := d.tokens()
			s = token.ReplaceAllStringFunc(s, func(t string) string {
				key := t[2 : len(t)-2]
				if key == "accounting.standard_cost" || key == "accounting.fast_cost" || key == "accounting.thorough_cost" || key == "profiles.default_variants" || key == "profiles.small_variants" || key == "profiles.thorough_variants" || key == "assistants.default_variants" || key == "assistants.small_variants" || key == "assistants.thorough_variants" {
					return values[key]
				}
				b, _ := json.Marshal(values[key])
				return string(b[1 : len(b)-1])
			})
		} else {
			s = d.Text(s, false, false)
		}
		out[name] = []byte(s)
	}
	for _, p := range d.Assistants {
		sources, _ := p.Sources(d.KnowledgeSources)
		out["agents/"+p.AgentName()+".md"] = p.RenderAgent(sources...)
	}
	for _, lang := range []string{"en", "de"} {
		n := d.Notice(lang)
		out["consent."+lang+".md"] = []byte("# " + n.Title + "\n\n- " + strings.Join(n.Points, "\n- ") + "\n\n" + n.Short + "\n")
	}
	return out
}

func Value(key string) string { return Current().Values[key] }
func Text(s string) string    { return Current().Text(s, false, false) }
func BrandPrefix() string     { return "/__" + Value("brand_id") + "/" }
func ConfigFile() string      { return "/home/user/.config/opencode/" + Value("config_file") }

func (d *Definition) CopyText(key, lang string) string {
	c := d.Copy[key]
	if lang == "en" {
		return d.Text(c.EN, false, false)
	}
	return d.Text(c.DE, false, false)
}

// Assistant resolves a reviewed definition; absent IDs never fall back to helper.
func (d *Definition) Assistant(id string) (assistants.Definition, bool) {
	for _, p := range d.Assistants {
		if p.ID == id {
			return p, true
		}
	}
	return assistants.Definition{}, false
}

func (d *Definition) AssistantPin(p assistants.Definition) assistants.Pin {
	sources, _ := p.Sources(d.KnowledgeSources)
	return p.Pin(map[string]string{"default": d.Models.Default, "small": d.Models.Small, "thorough": d.Models.Thorough}[p.ModelRole], sources...)
}
