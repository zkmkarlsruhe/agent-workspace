// SPDX-License-Identifier: MIT

package ncsync

import (
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// TestMetaTextLanguages: nextcloud.md is English for "en" and German otherwise.
func TestMetaTextLanguages(t *testing.T) {
	link := store.NCLink{NCFolder: "Agent Workspace/bericht", All: true}
	en, de := metaText(link, true, []string{"Team"}, true), metaText(link, true, []string{"Team"}, false)
	for _, want := range []string{"# Nextcloud in this project", "## Linked folder", "this project (except `", "## Collections", "only in “" + Root() + "/”"} {
		if !strings.Contains(en, want) {
			t.Errorf("en lacks %q:\n%s", want, en)
		}
	}
	for _, want := range []string{"# Nextcloud in diesem Projekt", "## Verknüpfter Ordner", "diesem Projekt (außer `", "## Sammlungen", "nur in „" + Root() + "/“"} {
		if !strings.Contains(de, want) {
			t.Errorf("de lacks %q:\n%s", want, de)
		}
	}
}
