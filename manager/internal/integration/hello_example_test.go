// SPDX-License-Identifier: MIT

package integration

import (
	"os"
	"reflect"
	"testing"
)

func TestHelloExampleManifest(t *testing.T) {
	f, err := os.Open("../../../examples/mcp-hello/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "hello" || m.Runs != "service" || m.HoldsPersonalData || len(m.Tools) != 1 || m.Tools[0] != "hello" {
		t.Fatalf("unexpected example contract: %+v", m)
	}
	inventory, err := Inventory("../../../deploy/example/mcp-hello/integrations")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inventory[len(inventory)-1], m) {
		t.Fatal("overlay manifest differs from example")
	}
	if _, err := Resolve([]Manifest{m}, Options{Enabled: map[string]bool{"hello": true}}); err != nil {
		t.Fatal(err)
	}
}
