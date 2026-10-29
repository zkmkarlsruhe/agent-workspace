// SPDX-License-Identifier: MIT

package convert

import (
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"os"
	"strings"
	"testing"
)

func TestIntegrationHouseStyle(t *testing.T) {
	b, err := os.ReadFile("../../../deploy/integrations/workspace-style/print.css")
	if err != nil || string(b) != houseCSS {
		t.Fatalf("house CSS snapshot differs: %v", err)
	}
	h, err := HTML([]byte("# Example"), "example")
	if err != nil || !strings.Contains(h, "Optional instance print style") {
		t.Fatalf("production style lost: %v", err)
	}
	d, err := instance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d.Integrations["workspace-style"] = false
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	h, err = HTML([]byte("# Example"), "example")
	if err != nil || strings.Contains(h, "Optional instance print style") {
		t.Fatalf("generic PDF requires house style: %v", err)
	}
}
