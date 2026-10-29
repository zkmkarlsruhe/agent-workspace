// SPDX-License-Identifier: MIT

package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenBucketRefillAndIsolation(t *testing.T) {
	var l Limiter
	now := time.Now()
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 60 {
		wg.Go(func() {
			if l.Allow("one", 30, time.Minute, now) {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if allowed.Load() != 30 {
		t.Fatal(allowed.Load())
	}
	if !l.Allow("two", 30, time.Minute, now) {
		t.Fatal("another uid blocked")
	}
	if l.Allow("one", 30, time.Minute, now.Add(time.Second)) {
		t.Fatal("refilled too early")
	}
	if !l.Allow("one", 30, time.Minute, now.Add(2*time.Second)) {
		t.Fatal("missing refill")
	}
	if l.Allow("one", 30, time.Minute, now.Add(2*time.Second)) {
		t.Fatal("extra refill")
	}
	for range 30 {
		if !l.Allow("one", 30, time.Minute, now.Add(2*time.Minute)) {
			t.Fatal("idle bucket not full")
		}
	}
	if len(l.buckets) != 1 {
		t.Fatal("idle bucket retained", l.buckets)
	}
}
