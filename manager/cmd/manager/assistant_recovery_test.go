// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
)

func TestResetAssistantToInstance(t *testing.T) {
	defaults, err := testinstance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Select(defaults)()
	st, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err = st.LoadAssistants(ctx); err != nil {
		t.Fatal(err)
	}
	p := defaults.Assistants[0]
	original := p.Title
	p.Title = "Edited"
	p.Enabled = true
	p.ConsentMode = "support"
	p.MCPTools = []string{defaults.MCPServer + ".web_read"}
	if _, err = st.SaveAssistant(ctx, p, defaults.AssistantPin(defaults.Assistants[0]).Hash, "admin"); err != nil {
		t.Fatal(err)
	}
	changed := *defaults
	changed.MCPServer = "replacement"
	restore := instance.Select(&changed)
	defer restore()
	if err = st.LoadAssistants(ctx); err != nil {
		t.Fatal(err)
	}
	if err = resetAssistant(ctx, st, &changed, p.ID); err != nil {
		t.Fatal(err)
	}
	saved, ok := instance.Current().Assistant(p.ID)
	if !ok || saved.Title != original || saved.Version != p.Version+2 {
		t.Fatal(saved)
	}
	stored, err := st.StoredAssistants(ctx)
	if err != nil || stored[0].ValidationError != "" {
		t.Fatal(stored, err)
	}
	if err = resetAssistant(ctx, st, &changed, "missing"); err == nil {
		t.Fatal("unknown reset accepted")
	}
}

func TestHostReloadDocumentationAuthentication(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/host/README.md")
	if os.IsNotExist(err) {
		t.Skip("host deployment documentation is excluded from public exports")
	}
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "curl ") && strings.Contains(line, "/admin/catalog/reload") {
			found = true
			if !strings.Contains(line, `-H "Authorization: Bearer ${OC_ADMIN_TOKEN:?set OC_ADMIN_TOKEN}"`) {
				t.Fatal(line)
			}
		}
	}
	if !found {
		t.Fatal("reload example missing")
	}
}
