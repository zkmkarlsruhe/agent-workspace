// SPDX-License-Identifier: MIT

package testinstance

import (
	"reflect"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/integration"
)

func TestRemoteFixturesLeaveShippedInventoryUnchanged(t *testing.T) {
	before, err := integration.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range d.Catalog {
		if entry.Kind == "mcp-central" && !d.RemoteEnabled(entry.ID) {
			t.Fatalf("remote fixture %s unavailable", entry.ID)
		}
	}
	after, err := integration.Builtins()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("fixture changed embedded inventory", err)
	}
	shipped, err := instance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, m := range before {
		known[m.Name] = true
	}
	for _, entry := range shipped.Catalog {
		if entry.Kind == "mcp-central" && !known[entry.ID] && shipped.RemoteEnabled(entry.ID) {
			t.Fatal("fixture escaped into shipped instance", entry.ID)
		}
	}
}
