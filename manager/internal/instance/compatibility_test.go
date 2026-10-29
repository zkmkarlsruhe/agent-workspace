// SPDX-License-Identifier: MIT

package instance

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestSchema1D74ab31Compatibility(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	raw, err := os.ReadFile("testdata/schema1-d74ab31.yaml")
	if os.IsNotExist(err) {
		t.Skip("historical private instance fixture is excluded from public exports")
	}
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.Timezone != "Europe/Berlin" {
		t.Fatal(d.Timezone)
	}
	for _, lang := range []string{"de", "en"} {
		if !strings.Contains(d.CopyText("folder_name_prompt", lang), "short German folder name") {
			t.Fatal("lost previous prompt")
		}
	}
	for _, key := range []string{"WARN", "timezone", "copy.folder_name_prompt"} {
		if !strings.Contains(logs.String(), key) {
			t.Fatalf("missing %s: %s", key, logs.String())
		}
	}
	// Explicit invalid values must still fail instead of silently falling back.
	if _, err := Parse(append(raw, []byte("\ntimezone: ''\n")...)); err == nil {
		t.Fatal("empty timezone accepted")
	}
}
