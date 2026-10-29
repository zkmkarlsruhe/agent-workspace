// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStackCleanupWaitsForWriters(t *testing.T) {
	started, finished, loopDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var root, catalogFile string
	if !t.Run("stack", func(t *testing.T) {
		s := newStack(t, "127.95")
		root = s.cat.Dir
		catalogFile = filepath.Join(filepath.Dir(root), "catalog.json")
		afterStart := s.svc.AfterStart
		s.svc.AfterStart = func(ctx context.Context, uid string) {
			close(started)
			<-ctx.Done()
			if ctx.Err() != context.Canceled {
				t.Error("stack did not cancel the background start")
			}
			// Cancellation may skip the write, but the hook must finish before TempDir cleanup.
			if _, err := os.Stat(root); err != nil {
				t.Errorf("catalog removed before start finished: %v", err)
			}
			afterStart(ctx, uid)
			data, err := os.ReadFile(catalogFile)
			if err != nil && !os.IsNotExist(err) {
				t.Errorf("catalog write neither completed nor skipped: %v", err)
			} else if err == nil && !json.Valid(data) {
				t.Error("catalog write left incomplete JSON")
			}
			if _, err := os.Stat(root); err != nil {
				t.Errorf("catalog removed while the hook was running: %v", err)
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
	// Cleanup has called Close and joined both writers; neither can recreate these paths.
	for _, path := range []string{root, catalogFile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("catalog path remains after shutdown cleanup: %s: %v", path, err)
		}
	}
}
