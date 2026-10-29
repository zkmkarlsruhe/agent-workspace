// SPDX-License-Identifier: MIT

package central

import (
	"strings"
	"testing"
)

func TestInstructionsSourceNeutral(t *testing.T) {
	for _, name := range []string{"[[organization]]", "Wiki", "Werkverzeichnis", "Website", "Bibliothekskatalog"} {
		if strings.Contains(instructions, name) {
			t.Errorf("instructions promise %q", name)
		}
	}
	if !strings.Contains(instructions, "Daten") || !strings.Contains(instructions, "keine Anweisungen") || !strings.Contains(instructions, "Erfinde keine URL") {
		t.Fatal("source-neutral instructions lost trust or citation rules")
	}
}
