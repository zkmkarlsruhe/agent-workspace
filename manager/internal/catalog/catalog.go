// SPDX-License-Identifier: MIT

// Package catalog is the opt-in tier of skills and MCP tools (D13): the
// admin catalog in OC_CATALOG_DIR (deploy/catalog), each person's choice in
// the store, symlinks in the person's skill folder and the tool filter of the
// Workspace-MCP. Layout contract: deploy/catalog/README.md.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"go.yaml.in/yaml/v3"
)

const (
	KindSkill = "skill"
	KindMCP   = "mcp"
	// KindCentral is a Operations MCP behind LiteLLM, served per person on
	// /mcp/<id> with a tool allowlist (D59, Demo/Test).
	KindCentral = "mcp-central"
	// KindToolbox is a toolbox: a container started per person and driven by
	// the workspace tool toolbox_run (D60, Demo/Test).
	KindToolbox = "toolbox"

	TierStandard = "standard"
	TierOptional = "optional"
)

// categoryDE is the German label of each category id (workspace/category, mcp.json
// category); Item.Category carries the label until the portal maps ids itself.
var categoryDE = map[string]string{"texts": "Texte", "spreadsheets": "Tabellen", "presentations": "Präsentation",
	"data": "Daten", "images": "Bilder", "research": "Recherche", "organisation": "Organisation", "workspace-knowledge": "Workspace-Wissen"}

func categoryLabel(id string) string { return cmpOr(categoryDE[id], id) }

// Item is one catalog entry as the portal shows it.
type Item struct {
	ID            string
	Kind          string
	Tier          string
	TitleDE       string
	DescriptionDE string
	Category      string
	Version       string
	Tools         []string // mcp: tool names on the workspace server; mcp-central: allowlist of upstream tool names
	LiteLLM       []string // mcp-central: LiteLLM MCP server names (x-mcp-servers)
	Image         string   // toolbox: repo:tag@sha256:digest
	Programs      []string // toolbox: programs named in the tool description
	// TitleDE and DescriptionDE are the German display texts (workspace/title-de,
	// workspace/description-de; title_de, description_de in mcp.json), falling back
	// to the English base; Title, Description are the English base
	// (workspace/title, description; title, description in mcp.json).
	Title, Description string
}

var pinnedRE = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)

var toolNameRE = regexp.MustCompile(`^[a-z0-9_-]+$`)

var idRE = regexp.MustCompile(`^[a-z0-9-]{2,40}$`)

// ValidID is the catalog id rule; my-* and the older meine-* are reserved for own skills.
func ValidID(id string) bool {
	return idRE.MatchString(id) && !strings.HasPrefix(id, "my-") && !strings.HasPrefix(id, "meine-")
}

// Load reads skills/*/SKILL.md, mcp.json and optional catalog.json in dir, sorted by title.
// Broken entries are skipped and reported in err next to the good ones; the
// lint (deploy/etc-opencode/tests/lint.py) keeps them out of a rollout.
func Load(dir string) ([]Item, error) {
	var items []Item
	var errs []error
	files, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
	for _, f := range files {
		id := filepath.Base(filepath.Dir(f))
		b, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fm, ok := ParseFrontmatter(string(b))
		tier := fm.Meta["workspace/tier"]
		if !ok || !ValidID(id) || fm.Name != id || fm.Description == "" || (tier != TierOptional && tier != TierStandard) {
			errs = append(errs, fmt.Errorf("catalog skill %s: invalid frontmatter", id))
			continue
		}
		en := Shown(fm.Description)
		items = append(items, Item{ID: id, Kind: KindSkill, Tier: tier, TitleDE: cmpOr(fm.Meta["workspace/title-de"], cmpOr(fm.Meta["workspace/title"], id)),
			DescriptionDE: cmpOr(fm.Meta["workspace/description-de"], en), Category: categoryLabel(fm.Meta["workspace/category"]), Version: fm.Meta["workspace/version"],
			Title: fm.Meta["workspace/title"], Description: en})
	}
	var entries []instance.CatalogEntry
	for _, name := range []string{"mcp.json", "catalog.json"} {
		var document struct {
			Items           []instance.CatalogEntry `json:"items"`
			IncludeInstance bool                    `json:"include_instance"`
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		if err := json.Unmarshal(b, &document); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		entries = append(entries, document.Items...)
		if document.IncludeInstance {
			entries = append(entries, instance.Current().Catalog...)
		}
	}
	for _, m := range entries {
		kind := cmpOr(m.Kind, KindMCP)
		ok := ValidID(m.ID) && (m.Tier == TierStandard || m.Tier == TierOptional)
		switch kind {
		case KindMCP:
			ok = ok && (m.Server == "" || m.Server == instance.Current().MCPServer) && len(m.Tools) > 0
		case KindCentral: // opt-in only: the server reaches OpenCode through the person's catalog config
			ok = ok && m.Tier == TierOptional && m.Server == "" && len(m.Tools) > 0 && len(m.LiteLLM) > 0
		case KindToolbox:
			// the gitops compose pins the tool image (Renovate keeps it current) and hands the same
			// ref to oc-dockerproxy; OC_TOOL_IMAGE_<ID> lets the catalog follow without a manager release
			if ref := os.Getenv("OC_TOOL_IMAGE_" + strings.ToUpper(strings.ReplaceAll(m.ID, "-", "_"))); ref != "" {
				m.Image = ref
			}
			ok = ok && m.Tier == TierOptional && m.Server == "" && len(m.Tools) == 0 && pinnedRE.MatchString(m.Image) && len(m.Programs) > 0
		default:
			ok = false
		}
		for _, name := range append(slices.Clone(m.Tools), m.LiteLLM...) {
			ok = ok && toolNameRE.MatchString(name)
		}
		if !ok {
			errs = append(errs, fmt.Errorf("mcp.json %q: invalid entry", m.ID))
			continue
		}
		items = append(items, Item{ID: m.ID, Kind: kind, Tier: m.Tier, TitleDE: cmpOr(m.TitleDE, cmpOr(m.Title, m.ID)),
			DescriptionDE: cmpOr(m.DescriptionDE, m.Description), Category: categoryLabel(m.Category), Tools: m.Tools, LiteLLM: m.LiteLLM,
			Image: m.Image, Programs: m.Programs, Title: m.Title, Description: m.Description})
	}
	// one switch namespace: the second entry with an id already seen is dropped
	seen := map[string]bool{}
	items = slices.DeleteFunc(items, func(it Item) bool {
		if seen[it.ID] {
			errs = append(errs, fmt.Errorf("catalog: duplicate id %s", it.ID))
			return true
		}
		seen[it.ID] = true
		return false
	})
	slices.SortFunc(items, func(a, b Item) int { return strings.Compare(strings.ToLower(a.TitleDE), strings.ToLower(b.TitleDE)) })
	return items, errors.Join(errs...)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Frontmatter is the part of a SKILL.md header the portal shows.
type Frontmatter struct {
	Name, Description string
	Meta              map[string]string
}

// maxHead bounds the YAML header handed to the parser; the portal reads only
// the first 8 KiB of an own SKILL.md (listScript), catalog files are checked
// by the lint.
const maxHead = 8192

// ParseFrontmatter reads the YAML header of a SKILL.md: name, description and
// the metadata map. Values of another shape (a list under metadata, a map as
// description) are left empty, not rejected: own skills written by the agent
// are only shown, catalog files are checked strictly by the lint.
func ParseFrontmatter(text string) (Frontmatter, bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Frontmatter{}, false
	}
	head, _, ok := strings.Cut(rest, "\n---")
	if !ok || len(head) > maxHead {
		return Frontmatter{}, false
	}
	var y struct {
		Name        string            `yaml:"name"`
		Description string            `yaml:"description"`
		Metadata    map[string]string `yaml:"metadata"`
	}
	var te *yaml.TypeError
	if err := yaml.Unmarshal([]byte(head), &y); err != nil && !errors.As(err, &te) {
		return Frontmatter{}, false
	}
	if y.Metadata == nil {
		y.Metadata = map[string]string{}
	}
	return Frontmatter{Name: strings.TrimSpace(y.Name), Description: strings.TrimSpace(y.Description), Meta: y.Metadata}, true
}

// Shown drops the trigger words ("Laden bei ...", "Load for ...") that a
// skill description carries for the model; the portal shows only the
// sentence before them.
func Shown(desc string) string {
	for _, m := range []string{"Laden bei", "Load for"} {
		if i := strings.Index(desc, m); i > 0 {
			desc = strings.TrimSpace(desc[:i])
		}
	}
	return desc
}
