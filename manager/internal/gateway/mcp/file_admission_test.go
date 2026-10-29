// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestHandwritingAdmissionBeforeRead(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	counted := &countedMediaFiles{Files: f}
	s.Files = counted
	s.Reserve = func(context.Context, string, float64) (func(), error) { return nil, llm.ErrDayCap }
	res, meta := s.fileTool(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", "handwriting_to_text", json.RawMessage(`{"path":"source.png"}`))
	if !res.IsError || meta.status != 429 || counted.reads.Load() != 0 {
		t.Fatal("refused handwriting read input", res, meta.status)
	}
}

func TestFilePreparationCapacityCancellation(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	counted := &countedMediaFiles{Files: f}
	s.Files = counted
	synctest.Test(t, func(t *testing.T) {
		previous := filePreparation
		filePreparation = make(chan struct{}, 1)
		defer func() { filePreparation = previous }()
		filePreparation <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				res, _ := s.fileTool(ctx, store.User{UID: "u1"}, "ses_abcdefgh12", "image_transform", json.RawMessage(`{"path":"source.png"}`))
				if !res.IsError {
					t.Error("cancelled preparation succeeded")
				}
			})
		}
		synctest.Wait()
		if counted.reads.Load() != 0 {
			t.Fatal("read before preparation capacity")
		}
		cancel()
		wg.Wait()
		<-filePreparation
		if counted.reads.Load() != 0 {
			t.Fatal("cancelled preparation read source")
		}
	})
}

func TestFileRateLimitBeforeReadAndRefill(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	counted := &countedMediaFiles{Files: f}
	s.Files = counted
	for range fileBurst {
		if !s.fileRates.allow("u1", time.Now()) {
			t.Fatal("burst refused")
		}
	}
	res, meta := s.fileTool(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", "image_transform", json.RawMessage(`{"path":"source.png"}`))
	if !res.IsError || meta.status != 429 || counted.reads.Load() != 0 {
		t.Fatal("rate limit read input")
	}
	now := time.Now()
	if !s.fileRates.allow("u2", now) {
		t.Fatal("one person exhausted another bucket")
	}
	if s.fileRates.allow("u1", now.Add(fileRefill/2)) {
		t.Fatal("premature refill")
	}
	if !s.fileRates.allow("u1", now.Add(fileRefill)) {
		t.Fatal("bucket did not refill")
	}
}
