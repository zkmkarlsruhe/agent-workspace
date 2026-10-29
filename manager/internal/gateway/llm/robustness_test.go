// SPDX-License-Identifier: MIT

package llm

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestTruncatedRequestBody(t *testing.T) {
	g := &Gateway{Users: users{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Log: slog.New(slog.DiscardHandler)}
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader("POST /v1/chat/completions HTTP/1.1\r\nHost: gateway\r\nContent-Length: 100\r\n\r\n{}")))
	if err != nil {
		t.Fatal(err)
	}
	r.RemoteAddr = "10.90.1.10:1"
	defer r.Body.Close()
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLLMBodyLimit(t *testing.T) {
	g := &Gateway{Users: users{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Log: slog.New(slog.DiscardHandler)}
	body := strings.Repeat(" ", 1<<20) + "invalid JSON"
	mb := 1
	g.MaxBodyMB = &mb
	if w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:1", body, nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("configured cap: %d", w.Code)
	}
	mb = 0
	if w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:1", body, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("disabled cap: %d %s", w.Code, w.Body.String())
	}
}

func TestBudgetIndependentUIDs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block, entered := make(chan struct{}), make(chan struct{})
		b := &Budget{CapEUR: 5, Now: time.Now, Spent: func(_ context.Context, uid string, _ time.Time) (float64, error) {
			if uid == "A" {
				close(entered)
				<-block
			}
			return 0, nil
		}}
		reserve := func(uid string, done chan error) {
			release, err := b.Reserve(context.Background(), uid)
			if err == nil {
				release()
			}
			done <- err
		}
		a, other := make(chan error, 1), make(chan error, 1)
		go reserve("A", a)
		<-entered
		go reserve("B", other)
		synctest.Wait()
		select {
		case err := <-other:
			if err != nil {
				t.Error(err)
			}
		default:
			t.Error("uid B waits on uid A's Spent")
		}
		close(block)
		if err := <-a; err != nil {
			t.Fatal(err)
		}
	})
}

func TestMeterUsageAfterLongLine(t *testing.T) {
	var events []store.UsageEvent
	m := &meter{accounting: instance.Accounting{UpstreamCostCurrency: "EUR"}, sse: true, done: func(e store.UsageEvent) { events = append(events, e) }}
	m.feed([]byte("data: " + strings.Repeat("x", maxScan)))
	if !m.skip || m.buf.Len() != 0 {
		t.Fatal("oversize line not skipped")
	}
	m.feed([]byte("tail\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"cost\":0.02}}\n\n"))
	m.finish()
	m.finish()
	if len(events) != 1 {
		t.Fatalf("billed %d times", len(events))
	}
	e := events[0]
	if e.InTokens == nil || *e.InTokens != 100 || e.OutTokens == nil || *e.OutTokens != 7 || e.CostEUR == nil || *e.CostEUR != 0.02 || e.CostSource != "usage" {
		t.Fatalf("usage after skipped line: %+v", e)
	}
}
