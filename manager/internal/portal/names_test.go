// SPDX-License-Identifier: MIT

package portal

import (
	"strings"
	"testing"
)

func TestCleanFilename(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"base name", "/folder/report.pdf", "report.pdf"},
		{"Windows path", `C:\folder\report.pdf`, "report.pdf"},
		{"hidden and control characters", " ..report\x00\n.txt ", "report.txt"},
		{"empty", "", ""},
		{"dots", "..", ""},
		{"200 runes", strings.Repeat("界", 196) + ".pdf", strings.Repeat("界", 196) + ".pdf"},
		{"keep 200-rune tail", "discard" + strings.Repeat("界", 196) + ".pdf", strings.Repeat("界", 196) + ".pdf"},
		{"tail starts with dots", "prefix.." + strings.Repeat("界", 194) + ".pdf", strings.Repeat("界", 194) + ".pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanFilename(tc.input); got != tc.want {
				t.Fatalf("cleanFilename(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
