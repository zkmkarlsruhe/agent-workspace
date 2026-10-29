// SPDX-License-Identifier: MIT

package consent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The support pilot changes the notice meaning; both languages share its version.
func TestVersionStableAcrossLanguages(t *testing.T) {
	if Version() != "c08cd573e4b6" {
		t.Fatalf("Version() %s changed: everyone would be asked again", Version())
	}
	de, en := Text("de"), Text("en")
	// the German text is still byte for byte the one the pinned hash was made of
	h := sha256.Sum256([]byte(de.Title + "\n" + strings.Join(de.Points, "\n")))
	if hex.EncodeToString(h[:6]) != Version() {
		t.Fatal("German notice changed: bump Version() by hand or restore the text")
	}
	// English wording belongs to the same deliberate consent revision.
	h = sha256.Sum256([]byte(en.Title + "\n" + strings.Join(en.Points, "\n")))
	if hex.EncodeToString(h[:6]) != "27fea8f7de79" {
		t.Fatal("English notice changed: restore it or deliberately revise consent")
	}
	if len(en.Points) != len(de.Points) || en.Title == de.Title || en.Short == de.Short {
		t.Fatal("English notice must translate every point")
	}
	if Text("fr").Title != de.Title {
		t.Fatal("unknown language must fall back to German")
	}
}
