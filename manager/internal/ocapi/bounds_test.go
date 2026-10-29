// SPDX-License-Identifier: MIT

package ocapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A container must not redirect manager requests to another host (SSRF).
func TestNoRedirect(t *testing.T) {
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/admin/catalog/reload", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Password: "pw", HTTP: &http.Client{}}
	if err := c.LocationReload(context.Background()); !IsStatus(err, 307) {
		t.Fatalf("err = %v, want HTTP 307", err)
	}
	if err := c.Events(context.Background(), func(Event) {}); !IsStatus(err, 307) {
		t.Fatalf("events err = %v, want HTTP 307", err)
	}
	if hit {
		t.Fatal("redirect was followed")
	}
}

type aaa struct{}

func (aaa) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":"`)
		io.Copy(w, io.LimitReader(aaa{}, maxBody+1))
		io.WriteString(w, `"}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Password: "pw"}
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestEventFrameLimit(t *testing.T) {
	line := "data: " + strings.Repeat("a", 1<<20) + "\n"
	r := strings.NewReader(strings.Repeat(line, 17) + "\n")
	if err := readEvents(r, func(Event) {}); err != errFrameTooLarge {
		t.Fatalf("err = %v, want errFrameTooLarge", err)
	}
}

// A container answering every page with a full page and the same (or an
// endless fresh) cursor must not keep the manager paging forever.
func TestSessionListAllBounded(t *testing.T) {
	page := `{"data":[` + strings.TrimSuffix(strings.Repeat(`{"id":"ses_attack000"},`, 200), ",") + `],"cursor":{"next":"%s"}}`
	for _, fresh := range []bool{false, true} {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			next := "x"
			if fresh {
				next = strings.Repeat("c", calls)
			}
			io.WriteString(w, strings.Replace(page, "%s", next, 1))
		}))
		c := &Client{BaseURL: srv.URL, Password: "pw", HTTP: &http.Client{}}
		if _, err := c.SessionListAll(context.Background(), "/workspace/p"); err == nil {
			t.Errorf("fresh=%v: endless paging accepted", fresh)
		}
		if calls > maxPages {
			t.Errorf("fresh=%v: %d pages", fresh, calls)
		}
		srv.Close()
	}
}

// A page with far more entries than asked for (a million empty objects, 3 MB,
// below maxBody) is refused while decoding, before it is held as sessions;
// so are pages that together exceed the byte budget.
func TestSessionPageEntriesBounded(t *testing.T) {
	var limits []string
	huge := `{"data":[{}` + strings.Repeat(`,{}`, 1_000_000) + `],"cursor":{"next":"p1"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limits = append(limits, r.URL.Query().Get("limit"))
		io.WriteString(w, huge)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Password: "pw", HTTP: &http.Client{}}
	if s, err := c.SessionListAll(context.Background(), "/workspace/p"); err == nil || len(limits) != 1 {
		t.Fatalf("page of %d sessions accepted (%d pages)", len(s), len(limits))
	}
	if s, err := c.SessionList(context.Background(), "/workspace/p", 0); err == nil {
		t.Fatalf("SessionList: page of %d sessions accepted", len(s))
	}
	if limits[0] != "200" {
		t.Fatalf("limit %q", limits[0])
	}

	calls := 0
	title := strings.Repeat("t", 100<<10)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"data":[`+strings.TrimSuffix(strings.Repeat(`{"title":"`+title+`"},`, 200), ",")+`],"cursor":{"next":"`+strings.Repeat("c", calls)+`"}}`)
	}))
	defer srv2.Close()
	c.BaseURL = srv2.URL
	if _, err := c.SessionListAll(context.Background(), "/workspace/p"); err == nil || calls*200*len(title) > 2*maxSessionBytes {
		t.Fatalf("byte budget: err %v after %d pages", err, calls)
	}
}
