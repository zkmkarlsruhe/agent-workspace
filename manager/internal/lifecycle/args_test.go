// SPDX-License-Identifier: MIT

package lifecycle

import (
	"os"
	"strings"
	"testing"
)

// TestOpenCodeArgsGolden: one line per log level, as runtime:smoke passes them
// to the real image. Regenerate with UPDATE_GOLDEN=1 after a deliberate change.
func TestOpenCodeArgsGolden(t *testing.T) {
	const path = "testdata/opencode-args.txt"
	var b strings.Builder
	for _, level := range []string{"info", "debug"} {
		b.WriteString(strings.Join(openCodeArgs("https://example.org", level), " ") + "\n")
	}
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != b.String() {
		t.Fatalf("%s is stale:\n%s\nwant:\n%s", path, want, b.String())
	}
}
