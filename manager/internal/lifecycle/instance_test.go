// SPDX-License-Identifier: MIT

package lifecycle

import (
	"os"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestMain(m *testing.M) {
	d, err := instance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		panic(err)
	}
	instance.Select(d)
	os.Exit(m.Run())
}
