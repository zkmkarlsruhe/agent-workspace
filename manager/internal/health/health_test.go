// SPDX-License-Identifier: MIT

package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func get(h *Handler) (int, report) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	var rep report
	json.NewDecoder(w.Body).Decode(&rep)
	return w.Code, rep
}

func TestHealth(t *testing.T) {
	ok := func(context.Context) error { return nil }
	var storeErr error
	h := &Handler{DiskPath: t.TempDir(), Running: func() int { return 3 }, Checks: map[string]func(context.Context) error{
		"store": func(context.Context) error { return storeErr }, "engine": ok,
		"catalog": DirReadable(t.TempDir()),
		// a hanging dependency is cut off by the budget, not by the Gatus timeout
		"pdf": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	}, Optional: map[string]bool{"pdf": true, "catalog": true}}
	t0 := time.Now()
	code, rep := get(h)
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("took %v", d)
	}
	// pdf is down but optional: reported, ok stays true
	if code != 200 || !rep.OK || rep.Checks["pdf"] == "ok" || rep.Checks["store"] != "ok" || rep.Checks["catalog"] != "ok" ||
		!slices.Equal(rep.Optional, []string{"catalog", "pdf"}) ||
		rep.Running != 3 || rep.DiskFreeBytes == 0 || rep.LastBackupAgeSecs != nil {
		t.Fatalf("%d %+v", code, rep)
	}
	storeErr = errors.New("attempt to write a readonly database")
	if code, _ := get(h); code != 200 {
		t.Fatal("result must be cached for 15 s", code)
	}
	h.at = time.Time{}
	if code, rep := get(h); code != 503 || rep.OK || rep.Checks["store"] != storeErr.Error() {
		t.Fatalf("%d %+v", code, rep)
	}
	// a critical check fails the same way when nothing is optional
	storeErr, h.at, h.Optional = nil, time.Time{}, nil
	if code, rep := get(h); code != 503 || rep.OK || rep.Optional != nil {
		t.Fatalf("%d %+v", code, rep)
	}
}

func TestChecks(t *testing.T) {
	ctx := context.Background()
	if DirReadable(filepath.Join(t.TempDir(), "missing"))(ctx) == nil {
		t.Error("missing dir")
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o644)
	if DirReadable(f)(ctx) == nil {
		t.Error("a file is no catalog dir")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	if err := HTTPOK(srv.Client(), srv.URL+"/health")(ctx); err != nil {
		t.Error(err)
	}
	if HTTPOK(srv.Client(), srv.URL+"/down")(ctx) == nil {
		t.Error("503 must fail")
	}
}
