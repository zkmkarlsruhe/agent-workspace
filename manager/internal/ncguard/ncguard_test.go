// SPDX-License-Identifier: MIT

package ncguard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestCheck(t *testing.T) {
	base, _ := url.Parse("https://cloud.example")
	const f = "https://cloud.example/remote.php/dav/files/z/"
	const in = f + "Agent%20Workspace/bericht/a.pdf"
	type tc struct {
		method, url string
		hdr         map[string]string
	}
	ok := []tc{
		{"PROPFIND", "https://cloud.example/remote.php/dav/files/z", nil},
		{"PROPFIND", f + "Team/Presse", nil},
		{"GET", f + "Privat/brief.docx", nil},
		{"GET", f + "100%25%20fertig.txt", nil},
		{"SEARCH", "https://cloud.example/remote.php/dav/", nil},
		{"MKCOL", f + "Agent%20Workspace", nil},
		{"MKCOL", f + "Agent%20Workspace/bericht", nil},
		{"PUT", in, map[string]string{"If-None-Match": "*"}},
		{"PUT", in, map[string]string{"If-Match": `"e1"`}},
		{"MOVE", f + "Agent%20Workspace/alt", map[string]string{"Destination": f + "Agent%20Workspace/neu", "Overwrite": "F"}},
		{"GET", "https://cloud.example/ocs/v2.php/cloud/user?format=json", nil},
		{"DELETE", "https://cloud.example/ocs/v2.php/core/apppassword?format=json", nil},
	}
	bad := []tc{
		// other parts of the account
		{"PROPFIND", "https://cloud.example/remote.php/dav/calendars/z/", nil},
		{"PROPFIND", "https://cloud.example/remote.php/dav/addressbooks/users/z/", nil},
		{"REPORT", "https://cloud.example/remote.php/dav/calendars/z/personal/", nil},
		{"GET", "https://cloud.example/ocs/v2.php/apps/files_sharing/api/v1/shares", nil},
		{"POST", "https://cloud.example/ocs/v2.php/apps/files_sharing/api/v1/shares", nil},
		{"GET", "https://cloud.example/ocs/v2.php/apps/spreed/api/v4/room", nil},
		{"GET", "https://cloud.example/index.php/apps/deck/api/v1.0/boards", nil},
		{"GET", "https://cloud.example/remote.php/webdav/Privat/a.txt", nil},
		{"GET", "https://cloud.example/remote.php/dav/trashbin/z/trash", nil},
		{"GET", "https://cloud.example/remote.php/dav/files/", nil},
		{"DELETE", "https://cloud.example/ocs/v2.php/core/apppassword/../../apps/files_sharing/api/v1/shares", nil},
		// destructive or outside the transfer folder
		{"DELETE", in, nil},
		{"COPY", in, map[string]string{"Destination": f + "Agent%20Workspace/b.pdf", "Overwrite": "F"}},
		{"PROPPATCH", in, nil},
		{"LOCK", in, nil},
		{"PUT", in, nil},
		{"PUT", in, map[string]string{"If-Match": "*"}},
		{"PUT", f + "Privat/a.pdf", map[string]string{"If-None-Match": "*"}},
		{"PUT", f + "Agent%20Workspacex/a.pdf", map[string]string{"If-None-Match": "*"}},
		{"MKCOL", f + "Team", nil},
		{"MOVE", f + "Agent%20Workspace/alt", map[string]string{"Destination": f + "Privat/alt", "Overwrite": "F"}},
		{"MOVE", f + "Privat/alt", map[string]string{"Destination": f + "Agent%20Workspace/alt", "Overwrite": "F"}},
		{"MOVE", f + "Agent%20Workspace/alt", map[string]string{"Destination": f + "Agent%20Workspace/neu", "Overwrite": "T"}},
		{"MOVE", f + "Agent%20Workspace/alt", map[string]string{"Destination": f + "Agent%20Workspace/neu"}},
		{"MOVE", f + "Agent%20Workspace/alt", map[string]string{"Destination": "https://evil.example/remote.php/dav/files/z/Agent%20Workspace/neu", "Overwrite": "F"}},
		// traversal and encodings
		{"PUT", f + "Agent%20Workspace/../Privat/a.pdf", map[string]string{"If-None-Match": "*"}},
		{"PUT", f + "Agent%20Workspace/%2e%2e/Privat/a.pdf", map[string]string{"If-None-Match": "*"}},
		{"PUT", f + "Agent%20Workspace/%2E%2E/Privat/a.pdf", map[string]string{"If-None-Match": "*"}},
		{"PUT", f + "Agent%20Workspace%2f..%2fPrivat/a.pdf", map[string]string{"If-None-Match": "*"}},
		{"PUT", f + "Agent%20Workspace/a%5c..%5cb", map[string]string{"If-None-Match": "*"}},
		{"PUT", f + "Agent%20Workspace/a%00b", map[string]string{"If-None-Match": "*"}},
		{"PUT", f + "Agent%20Workspace//a.pdf", map[string]string{"If-None-Match": "*"}},
		{"GET", f + "./a.pdf", nil},
		{"GET", "https://cloud.example/remote.php/dav/files/z/../../calendars/z/", nil},
		{"GET", "https://cloud.example/remote.php/dav/files/z/a.pdf?x=1", nil},
		{"SEARCH", "https://cloud.example/remote.php/dav", nil},
		// another host or scheme
		{"GET", "https://evil.example/remote.php/dav/files/z/a.pdf", nil},
		{"GET", "http://cloud.example/remote.php/dav/files/z/a.pdf", nil},
		{"GET", "https://cloud.example:8443/remote.php/dav/files/z/a.pdf", nil},
	}
	req := func(c tc) *http.Request {
		r, err := http.NewRequest(c.method, c.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	for _, c := range ok {
		if err := Check(base, req(c)); err != nil {
			t.Errorf("%s %s %v: refused", c.method, c.url, c.hdr)
		}
	}
	for _, c := range bad {
		if err := Check(base, req(c)); !errors.Is(err, ErrRefused) {
			t.Errorf("%s %s %v: allowed", c.method, c.url, c.hdr)
		}
	}
}

// TestTransport: a credentialed request that Check refuses never reaches the
// server; the Login Flow v2 without credentials passes.
func TestTransport(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := &http.Client{Transport: Transport{Base: base}}
	r, _ := http.NewRequest("PROPFIND", srv.URL+"/remote.php/dav/calendars/z/", nil)
	r.SetBasicAuth("z", "pw")
	if _, err := c.Do(r); !errors.Is(err, ErrRefused) || hits != 0 {
		t.Fatalf("calendar: %v, %d hits", err, hits)
	}
	r, _ = http.NewRequest("POST", srv.URL+"/index.php/login/v2", nil)
	if resp, err := c.Do(r); err != nil || hits != 1 {
		t.Fatalf("login flow: %v %d", err, hits)
	} else {
		resp.Body.Close()
	}
	r, _ = http.NewRequest("GET", srv.URL+"/remote.php/dav/files/z/a.txt", nil)
	r.SetBasicAuth("z", "pw")
	if resp, err := c.Do(r); err != nil || hits != 2 {
		t.Fatalf("file read: %v %d", err, hits)
	} else {
		resp.Body.Close()
	}
}

func TestMoveSameUser(t *testing.T) {
	base, _ := url.Parse("https://cloud.example")
	for _, destinationUser := range []string{"alice", "bob"} {
		r := httptest.NewRequest("MOVE", base.String()+"/remote.php/dav/files/alice/"+url.PathEscape(Root())+"/p/x", nil)
		r.Header.Set("Overwrite", "F")
		r.Header.Set("Destination", base.String()+"/remote.php/dav/files/"+destinationUser+"/"+url.PathEscape(Root())+"/p/y")
		if err := Check(base, r); (err == nil) != (destinationUser == "alice") {
			t.Errorf("destination user %s: %v", destinationUser, err)
		}
	}
}
