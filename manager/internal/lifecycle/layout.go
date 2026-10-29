// SPDX-License-Identifier: MIT

package lifecycle

import "github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"

// Layout keeps persistent folder names stable for the selected instance.
func Layout() instance.Folders { return instance.Current().Folders }
