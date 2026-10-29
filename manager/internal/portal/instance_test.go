// SPDX-License-Identifier: MIT

package portal

import (
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
	"os"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestMain(m *testing.M) {
	d, err := testinstance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		panic(err)
	}
	instance.Select(d)
	os.Exit(m.Run())
}
