// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"io"
)

// Shell runs script with args as uid 1000 in the person's running container,
// for packages outside lifecycle that need a file operation (internal/catalog).
// Exit 3 = ErrNotFound, 4 = ErrExists, stopped = ErrNotRunning.
func (s *Service) Shell(ctx context.Context, uid, script string, stdout io.Writer, args ...string) error {
	if stdout == nil {
		stdout = io.Discard
	}
	return s.sh(ctx, uid, script, nil, stdout, args...)
}
