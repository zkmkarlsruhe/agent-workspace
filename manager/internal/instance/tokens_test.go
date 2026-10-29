// SPDX-License-Identifier: MIT

package instance

import (
	"maps"
	"reflect"
	"testing"
)

func TestSelectedTokensCached(t *testing.T) {
	d, err := Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Definitions remain editable until selection.
	d.Values["name"] = "Example"
	before := d.tokens()
	defer Select(d)()
	first, second := d.tokens(), d.tokens()
	if !maps.Equal(before, first) {
		t.Fatal("selection changed tokens")
	}
	if reflect.ValueOf(first).Pointer() != reflect.ValueOf(second).Pointer() {
		t.Fatal("tokens rebuilt")
	}
	if d.Text("[[name]]", false, false) != "Example" {
		t.Fatal("stale token")
	}
}
