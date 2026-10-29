// SPDX-License-Identifier: MIT

package settings

import (
	"errors"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// AgentsPath is the person's own AGENTS.md (persistent config mount).
const AgentsPath = "/home/user/.config/opencode/AGENTS.md"

// MaxAgents is the limit of the whole file (deploy/etc-opencode/AGENTS.md: "unter 8 KiB").
const MaxAgents = 8 << 10

// The block is written with the English markers in every language. Files
// written before the English rename carry the German markers
// ("<!-- legacy-workspace:profil Anfang. ... -->" ... "<!-- legacy-workspace:profil Ende -->");
// Merge finds and replaces those too, permanently, so an old block is never
// left behind as a duplicate.
const (
	markerStem   = "<!-- legacy-workspace:profil" // common start of the old and the new markers
	markerPrefix = markerStem + "e"
	markerStart  = markerPrefix + " start. The portal maintains this section under Profile; changes here are overwritten. -->"
	markerEnd    = markerPrefix + " end -->"
	oldMarkerEnd = markerStem + " Ende -->"
)

var (
	ErrTooLarge = errors.New("profil: AGENTS.md would exceed 8 KiB")
	ErrBroken   = errors.New("profil: AGENTS.md has a start marker without end marker")
)

// Block is the managed part of AGENTS.md in lang; arbeitsweise "" falls back
// to the template of a. The markers are the same in every language, so
// Merge finds a block written before a language switch.
func Block(f Facts, a Answers, workingStyle, lang string) string {
	if workingStyle == "" {
		workingStyle = Template(a, lang)
	}
	return startMarker() + "\n" + AboutMe(f, lang) + "\n" + tx(lang, "## How I work") + "\n" + workingStyle + endMarker() + "\n"
}

// Merge puts block into the file content: in place of an existing managed
// block (new or old markers), else at the top (above "Gemerkt", which /merken
// appends at the end). Everything outside the markers stays byte for byte.
func Merge(file, block string) (string, error) {
	insert := -1
	for {
		i := -1
		for _, stem := range []string{markerStem, "<!-- " + instance.Value("profile_marker") + ":profil"} {
			if j := strings.Index(file, stem); j >= 0 && (i < 0 || j < i) {
				i = j
			}
		}
		if i < 0 {
			break
		}
		end := -1
		for _, m := range []string{markerEnd, oldMarkerEnd, endMarker()} {
			if j := strings.Index(file[i:], m); j >= 0 && (end < 0 || i+j+len(m) < end) {
				end = i + j + len(m)
			}
		}
		if end < 0 {
			return "", ErrBroken
		}
		if insert < 0 {
			insert = i
		}
		if end < len(file) && file[end] == '\n' {
			end++
		}
		file = file[:i] + file[end:]
	}
	var out string
	if insert >= 0 {
		out = file[:insert] + block + file[insert:]
	} else if strings.TrimSpace(file) == "" {
		out = block
	} else {
		out = block + "\n" + file
	}
	if len(out) > MaxAgents {
		return "", ErrTooLarge
	}
	return out, nil
}

func startMarker() string {
	return "<!-- " + instance.Value("profile_marker") + ":profile start. The portal maintains this section under Profile; changes here are overwritten. -->"
}

func endMarker() string { return "<!-- " + instance.Value("profile_marker") + ":profile end -->" }
