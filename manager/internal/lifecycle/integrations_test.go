// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"testing"
)

func TestIntegrationReadinessEmpty(t *testing.T) {
	d, err := instance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	// A nil client proves that an empty selection never polls the runtime.
	if err := (&Service{}).waitMCP(context.Background(), nil, "/workspace/p"); err != nil {
		t.Fatal(err)
	}
}
