// SPDX-License-Identifier: MIT

package ocapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient(t *testing.T) {
	var lastWrite string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "pw" {
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/info":
			io.WriteString(w, `{"version":"2.2.0","pid":7,"hostname":"oc-user1"}`)
		case "GET /api/session/active":
			io.WriteString(w, `{"data":{"ses_a":{"type":"running"}}}`)
		case "POST /api/session":
			if r.URL.RawQuery != "" {
				t.Errorf("session create must not use the query: %q", r.URL.RawQuery)
			}
			var in struct {
				Location Location `json:"location"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			dir := in.Location.Directory
			if dir == "/workspace/wrong" {
				dir = "/workspace" // what OpenCode does when it ignores the location
			}
			json.NewEncoder(w).Encode(map[string]any{"data": Session{ID: "ses_1", Location: Location{dir}}})
		case "POST /api/experimental/fs/write":
			b, _ := io.ReadAll(r.Body)
			lastWrite = r.URL.Query().Get("location[directory]") + "|" + r.URL.Query().Get("path") + "|" + string(b)
			io.WriteString(w, `{}`)
		case "GET /api/project":
			io.WriteString(w, `[{"id":"p","canonical":"/workspace/a","vcs":"git"}]`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := &Client{BaseURL: srv.URL, Password: "pw"}

	if i, err := c.Info(ctx); err != nil || i.Version != "2.2.0" || i.PID != 7 {
		t.Fatal(i, err)
	}
	if ids, err := c.Active(ctx); err != nil || len(ids) != 1 || ids[0] != "ses_a" {
		t.Fatal(ids, err)
	}
	if s, err := c.SessionCreate(ctx, "/workspace/bericht", "t"); err != nil || s.ID != "ses_1" {
		t.Fatal(s, err)
	}
	if _, err := c.SessionCreate(ctx, "/workspace/wrong", ""); err == nil {
		t.Fatal("directory mismatch must fail")
	}
	if err := c.FSWrite(ctx, "/workspace/b", "/workspace/b/x.md", []byte("hi")); err != nil || lastWrite != "/workspace/b|/workspace/b/x.md|hi" {
		t.Fatal(lastWrite, err)
	}
	if ps, err := c.Projects(ctx); err != nil || ps[0].Canonical != "/workspace/a" {
		t.Fatal(ps, err)
	}
	bad := &Client{BaseURL: srv.URL, Password: "nope"}
	if _, err := bad.Info(ctx); !IsStatus(err, 401) {
		t.Fatal(err)
	}
}
