// SPDX-License-Identifier: MIT

package lctest

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestCloseReapsDetachedExec(t *testing.T) {
	rt := NewRuntime()
	t.Cleanup(rt.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := rt.Exec(ctx, "oc-user1", []string{"sh", "-c", "sleep 60"}, nil, io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec: %v, want canceled attach", err)
	}
	rt.Close()
	if n := rt.Execs("oc-user1"); n != 0 {
		t.Fatalf("%d execs remain after Close", n)
	}
}
