// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStackCleanupWaitsForWriters(t *testing.T) {
	started, finished, loopDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var root string
	if !t.Run("stack", func(t *testing.T) {
		s := newStack(t, "127.95")
		root = s.cat.Dir
		afterStart := s.svc.AfterStart
		s.svc.AfterStart = func(ctx context.Context, uid string) {
			close(started)
			<-ctx.Done()
			if ctx.Err() != context.Canceled {
				t.Error("stack did not cancel the background start")
			}
			// Finish a catalog write during shutdown, before TempDir removes it.
			if _, err := os.Stat(root); err != nil {
				t.Errorf("catalog removed before start finished: %v", err)
			}
			afterStart(context.WithoutCancel(ctx), uid)
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), "catalog.json")); err != nil {
				t.Errorf("catalog write during shutdown failed: %v", err)
			}
			close(finished)
		}
		s.workers.Go(func() {
			<-s.ctx.Done()
			close(loopDone)
		})
		s.do(t, call{url: "/", as: &alice})
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("background start did not reach the catalog hook")
		}
	}) {
		return
	}
	for _, done := range []chan struct{}{finished, loopDone} {
		select {
		case <-done:
		default:
			t.Error("stack cleanup returned before a worker finished")
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("catalog temp directory remains: %v", err)
	}
}
