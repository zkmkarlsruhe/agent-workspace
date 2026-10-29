// SPDX-License-Identifier: MIT

package catalog

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

func TestItemsCacheUntilReload(t *testing.T) {
	for _, reload := range []string{"explicit", "admin", "start"} {
		t.Run(reload, func(t *testing.T) {
			dir := t.TempDir()
			s := &Service{Dir: dir, Log: slog.New(slog.DiscardHandler), Running: func() []string { return nil }, Client: func(string) (*ocapi.Client, bool) { return nil, false }}
			if len(s.Items()) != 0 {
				t.Fatal("expected empty catalog")
			}
			data, err := os.ReadFile("testdata/catalog/skills/minutes/SKILL.md")
			if err != nil {
				t.Fatal(err)
			}
			skill := filepath.Join(dir, "skills", "minutes")
			if err := os.MkdirAll(skill, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if len(s.Items()) != 0 {
				t.Fatal("catalog reread without invalidation")
			}
			switch reload {
			case "explicit":
				s.Reload()
			case "admin":
				s.ReloadAll(context.Background())
			case "start":
				s.AfterStart(context.Background(), "user")
			}
			items := s.Items()
			if len(items) != 1 || items[0].ID != "minutes" {
				t.Fatal(items)
			}
			items[0].ID = "mutated"
			if s.Items()[0].ID != "minutes" {
				t.Fatal("caller mutated cached items")
			}
		})
	}
}
