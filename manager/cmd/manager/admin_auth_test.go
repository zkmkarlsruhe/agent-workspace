// SPDX-License-Identifier: MIT

package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
)

func TestAdminToken(t *testing.T) {
	cat := &catalog.Service{Running: func() []string { return nil }, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	mux.Handle("POST /admin/catalog/reload", catalogReload(cat))
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /admin/future", func(http.ResponseWriter, *http.Request) {})
	for _, tt := range []struct {
		name, token, header, method, path, from string
		want                                    int
	}{
		{"missing", "secret", "", "POST", "/admin/catalog/reload", "192.0.2.1:1", 401},
		{"wrong", "secret", "Bearer wrong", "POST", "/admin/catalog/reload", "192.0.2.1:1", 401},
		{"bare", "secret", "secret", "POST", "/admin/catalog/reload", "192.0.2.1:1", 401},
		{"valid", "secret", "Bearer secret", "POST", "/admin/catalog/reload", "192.0.2.1:1", 200},
		{"empty token", "", "", "POST", "/admin/catalog/reload", "192.0.2.1:1", 401},
		{"empty bearer", "", "Bearer ", "POST", "/admin/future", "192.0.2.1:1", 401},
		{"empty token health", "", "", "GET", "/health", "192.0.2.1:1", 200},
		{"health", "secret", "", "GET", "/health", "192.0.2.1:1", 200},
		{"future route", "secret", "", "POST", "/admin/future", "192.0.2.1:1", 401},
		{"user network", "secret", "Bearer secret", "POST", "/admin/catalog/reload", "10.90.1.10:1", 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := notFromUserNets(adminAuth(mux, tt.token), config.Config{UserNetPrefix: "10.90"})
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r.RemoteAddr = tt.from
			r.Header.Set("Authorization", tt.header)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
