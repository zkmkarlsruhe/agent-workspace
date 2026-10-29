// SPDX-License-Identifier: MIT

package ncsync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWritable(t *testing.T) {
	ok := map[string]string{
		"Agent Workspace":                "Agent Workspace",
		"/Agent Workspace/":              "Agent Workspace",
		" Agent Workspace/bericht/a.pdf": "Agent Workspace/bericht/a.pdf",
		"Agent Workspace/100% fertig":    "Agent Workspace/100% fertig",
	}
	for in, want := range ok {
		if got, k := Writable(in); !k || got != want {
			t.Errorf("Writable(%q) = %q %v, want %q", in, got, k, want)
		}
	}
	for _, in := range []string{"", "/", "Team/Presse", "/Team", "Agent Workspacex", "Agent Workspacex/a", "agents am workspace/a",
		"Agent Workspace/../Team", "Agent Workspace/./a", "Agent Workspace//a", "Agent Workspace/%2e%2e/Team", "Agent Workspace/%2E%2e",
		"Agent Workspace/a%2fb", "Agent Workspace/.project", "Agent Workspace/%2eproject", "Agent Workspace/a\x00b", `Agent Workspace\..\Team`,
		"../Agent Workspace", "Agent Workspace/" + strings.Repeat("a", 1100)} {
		if got, k := Writable(in); k {
			t.Errorf("Writable(%q) = %q, want refused", in, got)
		}
	}
}

// TestDAVNeverDestroys: the client itself refuses every write outside the
// transfer folder, unconditional PUTs and methods with side effects beyond
// MKCOL, PUT and MOVE; none of them reaches the server (D58).
func TestDAVNeverDestroys(t *testing.T) {
	dav := newFakeDAV()
	srv := httptest.NewServer(dav)
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	d, c, ctx := NewDAV(base), Creds{"z", "z", "pw"}, context.Background()
	in := Root() + "/bericht/a.pdf"
	checks := map[string]error{
		"put outside": func() error {
			_, err := d.Put(ctx, c, "Team/a.pdf", strings.NewReader("x"), "", time.Time{})
			return err
		}(),
		"put encoded": func() error {
			_, err := d.Put(ctx, c, Root()+"/%2e%2e/a.pdf", strings.NewReader("x"), "", time.Time{})
			return err
		}(),
		"put star":        func() error { _, err := d.Put(ctx, c, in, strings.NewReader("x"), "*", time.Time{}); return err }(),
		"put empty tag":   func() error { _, err := d.Put(ctx, c, in, strings.NewReader("x"), `""`, time.Time{}); return err }(),
		"mkcol outside":   d.Mkdirs(ctx, c, "Team/neu"),
		"mkcol traversal": d.Mkdirs(ctx, c, Root()+"/../Team"),
		"move from":       d.Move(ctx, c, "Team/Presse", Root()+"/Presse"),
		"move to":         d.Move(ctx, c, Root()+"/bericht", "Team/bericht"),
		"delete":          func() error { _, err := d.do(ctx, c, http.MethodDelete, d.filesURL(c.User, in), nil, nil); return err }(),
		"copy":            func() error { _, err := d.do(ctx, c, "COPY", d.filesURL(c.User, in), nil, nil); return err }(),
		"proppatch":       func() error { _, err := d.do(ctx, c, "PROPPATCH", d.filesURL(c.User, in), nil, nil); return err }(),
		"post":            func() error { _, err := d.do(ctx, c, http.MethodPost, d.filesURL(c.User, in), nil, nil); return err }(),
	}
	for name, err := range checks {
		if !errors.Is(err, ErrOutside) {
			t.Errorf("%s: %v, want ErrOutside", name, err)
		}
	}
	if len(dav.calls) != 0 {
		t.Fatalf("refused requests reached Nextcloud: %v", dav.calls)
	}
	// allowed writes are conditional and stay inside
	if err := d.Mkdirs(ctx, c, Root()+"/bericht"); err != nil {
		t.Fatal(err)
	}
	tag, err := d.Put(ctx, c, in, strings.NewReader("1"), "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Put(ctx, c, in, strings.NewReader("2"), "", time.Time{}); HTTPStatus(err) != 412 {
		t.Fatalf("second create must not overwrite: %v", err)
	}
	if _, err := d.Put(ctx, c, in, strings.NewReader("2"), tag, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(dav.bad) != 0 {
		t.Fatal(dav.bad)
	}
}

// TestParseHrefs: names come from hrefs below the user root only; a decoded
// %2e%2e is cleaned before the check, a neighbour user root ("z2") is dropped.
func TestParseHrefs(t *testing.T) {
	base, _ := url.Parse("https://cloud.example")
	d := NewDAV(base)
	body := `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">` +
		`<d:response><d:href>/remote.php/dav/files/z/Agent%20Workspace/a.pdf</d:href></d:response>` +
		`<d:response><d:href>/remote.php/dav/files/z/Agent%20Workspace/%2e%2e/Privat/b.pdf</d:href></d:response>` +
		`<d:response><d:href>/remote.php/dav/files/z2/c.pdf</d:href></d:response>` +
		`<d:response><d:href>/remote.php/dav/files/z/%2e%2e/y/d.pdf</d:href></d:response>` +
		`<d:response><d:href>https://other.example/remote.php/dav/files/z/../../x</d:href></d:response>` +
		`</d:multistatus>`
	es, err := d.parse(strings.NewReader(body), "z")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range es {
		got = append(got, e.Path)
	}
	if strings.Join(got, "|") != "Agent Workspace/a.pdf|Privat/b.pdf" {
		t.Fatalf("%q", got)
	}
}
