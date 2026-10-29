// SPDX-License-Identifier: MIT

package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(NewHandler(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func TestRequestIDTrust(t *testing.T) {
	var got string
	h := Middleware(slog.New(slog.DiscardHandler), "edge", func(r *http.Request) bool { return r.RemoteAddr == "10.0.0.1:1" },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = ReqID(r.Context()) }))
	for _, c := range []struct {
		name, remote, in string
		keep             bool
	}{
		{"trusted sane", "10.0.0.1:1", "abc-123_DEF.456", true},
		{"untrusted", "10.0.0.2:1", "abc-123_DEF.456", false},
		{"trusted insane", "10.0.0.1:1", "a b\x00c<script>", false},
		{"trusted too short", "10.0.0.1:1", "x", false},
		{"trusted none", "10.0.0.1:1", "", false},
	} {
		r := httptest.NewRequest("GET", "/x", nil)
		r.RemoteAddr = c.remote
		if c.in != "" {
			r.Header.Set(HeaderReqID, c.in)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if (got == c.in) != c.keep || got == "" {
			t.Errorf("%s: req id %q, incoming %q", c.name, got, c.in)
		}
		if w.Header().Get(HeaderReqID) != got {
			t.Errorf("%s: response header %q, context %q", c.name, w.Header().Get(HeaderReqID), got)
		}
	}
}

func TestHandlerAddsFields(t *testing.T) {
	var buf bytes.Buffer
	log := newLog(&buf)
	ctx := Set(context.Background(), Fields{ReqID: "r1", UID: "u1"})
	ctx = With(ctx, Fields{Slot: 7, SessionID: "ses_1"})
	log.InfoContext(ctx, "x", "uid", "explicit")
	log.Info("no ctx")
	ls := lines(t, &buf)
	if ls[0]["req_id"] != "r1" || ls[0]["slot"] != float64(7) || ls[0]["session_id"] != "ses_1" || ls[0]["uid"] != "explicit" {
		t.Errorf("fields: %v", ls[0])
	}
	if strings.Count(buf.String(), `"uid"`) != 1 {
		t.Errorf("uid duplicated: %s", buf.String())
	}
	if _, ok := ls[1]["req_id"]; ok {
		t.Errorf("fields without ctx: %v", ls[1])
	}
}

func TestPanicRecovery(t *testing.T) {
	var buf bytes.Buffer
	h := Middleware(newLog(&buf), "gw", nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", w.Code)
	}
	ls := lines(t, &buf)
	if ls[0]["level"] != "ERROR" || ls[0]["panic"] != "boom" || !strings.Contains(ls[0]["stack"].(string), "logx") || ls[0]["req_id"] == nil {
		t.Errorf("panic line: %v", ls[0])
	}
	if ls[1]["msg"] != "http" || ls[1]["status"] != float64(500) {
		t.Errorf("access line: %v", ls[1])
	}
}

func TestAccessLogNoQuery(t *testing.T) {
	var buf bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		Set(r.Context(), Fields{UID: "u1"})
		io.WriteString(w, "hello")
	})
	h := Middleware(newLog(&buf), "edge", nil, Route(mux))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/projects/secret-name?ticket=s3cr3t&q=private", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health?x=s3cr3t", nil))
	out := buf.String()
	if strings.Contains(out, "s3cr3t") || strings.Contains(out, "secret-name") || strings.Contains(out, "private") {
		t.Fatalf("query or path id in access log: %s", out)
	}
	ls := lines(t, &buf)
	if ls[0]["route"] != "GET /projects/{name}" || ls[0]["status"] != float64(200) || ls[0]["bytes"] != float64(5) || ls[0]["uid"] != "u1" {
		t.Errorf("access line: %v", ls[0])
	}
	if ls[1]["level"] != "DEBUG" || ls[1]["route"] != "/health" {
		t.Errorf("health line: %v", ls[1])
	}
}

func TestTransportPropagatesAndLogsError(t *testing.T) {
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(HeaderReqID)
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, strings.Repeat("e", 600))
	}))
	defer up.Close()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(newLog(&buf))
	defer slog.SetDefault(old)
	ctx := Set(context.Background(), Fields{ReqID: "rid-12345678"})
	for _, prop := range []bool{true, false} {
		seen = ""
		req, _ := http.NewRequestWithContext(ctx, "GET", up.URL+"/p?key=s3cr3t", nil)
		resp, err := (&http.Client{Transport: Transport{Upstream: "test", Propagate: prop, Body: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(b) != 600 {
			t.Errorf("body cut for the caller: %d", len(b))
		}
		if (seen == "rid-12345678") != prop {
			t.Errorf("propagate %v: upstream saw %q", prop, seen)
		}
	}
	if strings.Contains(buf.String(), "s3cr3t") {
		t.Errorf("query in log: %s", buf.String())
	}
	l := lines(t, &buf)[0]
	if l["upstream"] != "test" || l["status"] != float64(502) || len(l["body"].(string)) != 512 || l["req_id"] != "rid-12345678" {
		t.Errorf("upstream line: %v", l)
	}
}

func TestTransportNoPath(t *testing.T) {
	var status int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer up.Close()
	old := slog.Default()
	defer slog.SetDefault(old)
	for _, code := range []int{200, 403, 404, 500} {
		status = code
		for _, noPath := range []bool{false, true} {
			var buf bytes.Buffer
			slog.SetDefault(newLog(&buf))
			req, _ := http.NewRequest("GET", up.URL+"/private-token?secret=query", nil)
			resp, err := (Transport{Upstream: "web", NoPath: noPath}).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			entries := lines(t, &buf)
			if len(entries) != 1 {
				t.Fatalf("logs: %s", buf.String())
			}
			_, hasPath := entries[0]["path"]
			if hasPath == noPath || noPath && strings.Contains(buf.String(), "private-token") || strings.Contains(buf.String(), "secret=query") {
				t.Fatalf("status=%d NoPath=%v: %s", code, noPath, buf.String())
			}
			if entries[0]["host"] == nil || entries[0]["status"] != float64(code) {
				t.Fatalf("missing metadata: %s", buf.String())
			}
		}
	}
}
