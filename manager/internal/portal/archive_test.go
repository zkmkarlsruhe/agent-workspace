// SPDX-License-Identifier: MIT

package portal

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestArchivedChatsAndRestore(t *testing.T) {
	h, f := setup(t)
	f.addProject("demo", "")
	f.projects["demo"].ArchivedChats = []Chat{{ID: "old", Title: "<script>old</script>", Archived: true}}
	path := "/projects/demo/chats/old/restore"
	for _, language := range []string{"de", "en"} {
		w := do(t, h, req{path: "/projects/demo", header: map[string]string{"Accept-Language": language}})
		wantStatus(t, w, http.StatusOK)
		wantBody(t, w, `id="archived-chats"`, `action="`+path+`"`, "&lt;script&gt;old&lt;/script&gt;")
		if language == "de" {
			wantBody(t, w, "Archivierte Chats", "Wiederherstellen")
		} else {
			wantBody(t, w, "Archived chats", "Restore")
		}
		if strings.Contains(w.Body.String(), "<script>old") {
			t.Fatal("unescaped title")
		}
	}
	for _, headers := range []map[string]string{
		{"Origin": "https://other.example"},
		{"Sec-Fetch-Site": "same-site", "Origin": "https://preview.apps.example.org"},
	} {
		wantStatus(t, do(t, h, req{method: "POST", path: path, header: headers}), http.StatusForbidden)
	}
	wantStatus(t, do(t, h, req{method: "POST", path: path, id: &Identity{}}), http.StatusForbidden)
	wantStatus(t, do(t, h, req{path: path}), http.StatusNotFound)
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "restore-session:") }) {
		t.Fatal("rejected request reached backend")
	}
	w := do(t, h, req{method: "POST", path: path, header: map[string]string{"Origin": "https://apps.example.org"}})
	wantStatus(t, w, http.StatusSeeOther)
	if w.Header().Get("Location") != "/projects/demo" || !slices.Contains(f.calls, "restore-session:"+anna.UID+":demo:old") {
		t.Fatal(w.Header(), f.calls)
	}
	w = do(t, h, req{path: "/projects/demo"})
	if strings.Contains(w.Body.String(), `id="archived-chats"`) || len(f.projects["demo"].ChatList) != 1 {
		t.Fatal("restored chat remains archived")
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/projects/missing/chats/old/restore"}), http.StatusNotFound)
	wantStatus(t, do(t, h, req{method: "POST", path: "/projects/demo/chats/unknown/restore"}), http.StatusNotFound)
}
