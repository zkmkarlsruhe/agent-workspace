// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
)

func TestNextcloudConcurrentClient(t *testing.T) {
	n := &Nextcloud{Base: mustURL("https://cloud.example")}
	start := make(chan struct{})
	clients := make(chan *http.Client, 2)
	for range 2 {
		go func() {
			<-start
			clients <- n.client()
		}()
	}
	close(start)
	first, second := <-clients, <-clients
	if first == nil || first != second {
		t.Fatal("concurrent calls did not share one client")
	}
	if err := first.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect policy: %v", err)
	}
}

func TestNCPath(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", " /Dokumente/ ": "Dokumente", "a b/c.docx": "a b/c.docx"} {
		if got, ok := ncPath(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"../x", "a/../../b", "a//b", "a/./b", "a\x00b", `a\..\b`} {
		if _, ok := ncPath(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

// TestParseListingHrefs: only children of dir below the user root; a
// neighbour root ("m s2", "m sx.txt") and a decoded %2e%2e are dropped.
func TestParseListingHrefs(t *testing.T) {
	body := `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">
<d:response><d:href>/remote.php/dav/files/m%20s/</d:href></d:response>
<d:response><d:href>/remote.php/dav/files/m%20s/a.txt</d:href></d:response>
<d:response><d:href>/remote.php/dav/files/m%20sx.txt</d:href></d:response>
<d:response><d:href>/remote.php/dav/files/m%20s2/b.txt</d:href></d:response>
<d:response><d:href>/remote.php/dav/files/m%20s/Dokumente/%2e%2e/%2e%2e/c.txt</d:href></d:response>
</d:multistatus>`
	es, err := parseListing(strings.NewReader(body), "/remote.php/dav/files/m s", "")
	if err != nil || len(es) != 1 || es[0].Name != "a.txt" {
		t.Fatalf("%+v %v", es, err)
	}
}

const ncListing = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
<d:response><d:href>/remote.php/dav/files/m%20s/Dokumente/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/remote.php/dav/files/m%20s/Dokumente/Bericht%202027.docx</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>2048</d:getcontentlength><d:getlastmodified>Fri, 16 Oct 2026 10:00:00 GMT</d:getlastmodified></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/remote.php/dav/files/m%20s/Dokumente/Fotos/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/remote.php/dav/files/other/geheim.txt</d:href><d:propstat><d:prop><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`

type ncFiles struct {
	got map[string]string
	err error
}

func (f *ncFiles) Upload(_ context.Context, uid, project, name string, r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if f.err != nil {
		return "", f.err
	}
	f.got[uid+"|"+project+"|"+name] = string(b)
	return name, nil
}

func (f *ncFiles) SessionDir(_ context.Context, _, id string) (string, error) {
	if id != "ses_abcdefgh12" {
		return "", errors.New("404")
	}
	return "/workspace/bericht", nil
}

func TestNextcloud(t *testing.T) {
	var methods []string
	nc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.EscapedPath())
		if u, p, _ := r.BasicAuth(); u != "m s" || p != "app-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "PROPFIND" && r.URL.Path == "/remote.php/dav/files/m s/Dokumente" && r.Header.Get("Depth") == "1":
			w.WriteHeader(http.StatusMultiStatus)
			io.WriteString(w, ncListing)
		case r.Method == "GET" && r.URL.Path == "/remote.php/dav/files/m s/Dokumente/Bericht 2027.docx":
			io.WriteString(w, "DOCX")
		case r.Method == "GET" && r.URL.Path == "/remote.php/dav/files/m s/umleitung.txt":
			http.Redirect(w, r, "http://evil.example/", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer nc.Close()
	base, _ := url.Parse(nc.URL)
	// user id "m s" for the WebDAV path, login name "m s" for Basic
	pw, connected, expired := "app-pw", true, 0
	creds := func(_ context.Context, uid string) (string, string, string, error) {
		if uid != "u1" || !connected {
			return "", "", "", errors.New("not connected")
		}
		return "m s", "m s", pw, nil
	}
	files := &ncFiles{got: map[string]string{}}
	r := &rec{}
	s := New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: r,
		Log: slog.New(slog.DiscardHandler), Nextcloud: &Nextcloud{Base: base, Creds: creds, Files: files,
			Expired: func(context.Context, string) { expired++ }}})
	const me = "10.90.1.10:40000"

	if w := rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); !strings.Contains(w.Body.String(), `"nextcloud_fetch"`) {
		t.Fatalf("tools/list: %s", w.Body)
	}
	call := func(name, args string) toolResult {
		w := rpc(s, me, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`,"_meta":{"ai.opencode/sessionID":"ses_abcdefgh12"}}}`)
		var resp struct{ Result toolResult }
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Result
	}

	res := call("nextcloud_list", `{"folder":"/Dokumente/"}`)
	txt := res.Content[0].Text
	if res.IsError || !strings.Contains(txt, "[Ordner] Fotos/") || !strings.Contains(txt, "Bericht 2027.docx (2 KB, 2026-10-16 12:00)") ||
		strings.Contains(txt, "geheim") || strings.Index(txt, "Fotos") > strings.Index(txt, "Bericht") {
		t.Fatalf("liste: %+v", res)
	}

	res = call("nextcloud_fetch", `{"path":"Dokumente/Bericht 2027.docx"}`)
	if res.IsError || files.got["u1|bericht|Bericht 2027.docx"] != "DOCX" || !strings.Contains(res.Content[0].Text, "/workspace/bericht/inbox/Bericht 2027.docx") {
		t.Fatalf("holen: %+v %v", res, files.got)
	}
	last := r.ev[len(r.ev)-1]
	if last.Capability != "nextcloud" || last.Target != "nextcloud.fetch" || last.Status != 200 || last.SessionID != "ses_abcdefgh12" {
		t.Errorf("usage %+v", last)
	}

	for _, c := range []struct{ tool, args, want string }{
		{"nextcloud_fetch", `{"path":"fehlt.txt"}`, "gibt es in deiner Nextcloud nicht"},
		{"nextcloud_fetch", `{"path":"umleitung.txt"}`, "nicht erreichbar"},
		{"nextcloud_fetch", `{"path":"../etc/passwd"}`, "Ungültiger Pfad"},
		{"nextcloud_fetch", `{"path":".env"}`, "Punkt"},
		{"nextcloud_fetch", `{"path":"a.txt","project":"../x"}`, "Projektname"},
		{"nextcloud_list", `{"folder":"Gibtsnicht"}`, "Ordner gibt es"},
	} {
		if res := call(c.tool, c.args); !res.IsError || !strings.Contains(res.Content[0].Text, c.want) {
			t.Errorf("%s %s: %+v", c.tool, c.args, res)
		}
	}
	if len(files.got) != 1 {
		t.Errorf("unexpected uploads %v", files.got)
	}

	files.err = lifecycle.ErrQuota
	if res := call("nextcloud_fetch", `{"path":"Dokumente/Bericht 2027.docx"}`); !strings.Contains(res.Content[0].Text, "Speicher ist voll") {
		t.Errorf("quota: %+v", res)
	}

	pw = "widerrufen"
	if res := call("nextcloud_list", `{}`); !strings.Contains(res.Content[0].Text, "unter Verbindungen neu verbinden") || expired != 1 {
		t.Errorf("401: %+v, expired %d", res, expired)
	}
	connected = false
	n := len(methods)
	if res := call("nextcloud_list", `{}`); !strings.Contains(res.Content[0].Text, "Nextcloud ist nicht verbunden") || len(methods) != n {
		t.Errorf("not connected: %+v, upstream calls %d", res, len(methods)-n)
	}

	off := New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: r, Log: slog.New(slog.DiscardHandler)})
	if w := rpc(off, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); strings.Contains(w.Body.String(), "nextcloud") {
		t.Error("nextcloud listed without OC_NEXTCLOUD_URL")
	}
}

// TestNextcloudLive runs against a real Nextcloud with a test account:
// OC_NC_IT=http://127.0.0.1:<port> OC_NC_LOGIN=... OC_NC_PW=<app password> OC_NC_DIR=<folder with files>
func TestNextcloudLive(t *testing.T) {
	raw := os.Getenv("OC_NC_IT")
	if raw == "" {
		t.Skip("OC_NC_IT not set")
	}
	base, _ := url.Parse(raw)
	files := &ncFiles{got: map[string]string{}}
	s := New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: &rec{},
		Log: slog.New(slog.DiscardHandler), Nextcloud: &Nextcloud{Base: base, Files: files,
			Creds: func(context.Context, string) (string, string, string, error) {
				return os.Getenv("OC_NC_LOGIN"), os.Getenv("OC_NC_LOGIN"), os.Getenv("OC_NC_PW"), nil
			}}})
	call := func(name, args string) toolResult {
		w := rpc(s, "10.90.1.10:40000", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`,"_meta":{"ai.opencode/sessionID":"ses_abcdefgh12"}}}`)
		var resp struct{ Result toolResult }
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Result
	}
	dir, _ := json.Marshal(os.Getenv("OC_NC_DIR"))
	res := call("nextcloud_list", `{"folder":`+string(dir)+`}`)
	t.Log(res.Content[0].Text)
	st, _ := res.Structured.(map[string]any)
	var entries []ncEntry
	b, _ := json.Marshal(st["entries"])
	json.Unmarshal(b, &entries)
	if res.IsError || len(entries) == 0 {
		t.Fatalf("liste: %+v", res)
	}
	for _, e := range entries {
		if e.Folder {
			continue
		}
		p, _ := json.Marshal(os.Getenv("OC_NC_DIR") + "/" + e.Name)
		res := call("nextcloud_fetch", `{"path":`+string(p)+`}`)
		t.Log(res.Content[0].Text)
		if res.IsError || int64(len(files.got["u1|bericht|"+e.Name])) != e.Bytes {
			t.Errorf("holen %s: %+v", p, res)
		}
	}
	if res := call("nextcloud_list", `{"folder":"gibt-es-nicht"}`); !res.IsError {
		t.Errorf("missing folder: %+v", res)
	}
}
