// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ncsync"
)

type fakeSync struct {
	calls []string
	err   error
}

func (f *fakeSync) Upload(_ context.Context, uid, project, rel, dir string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, "ablegen|"+project+"|"+rel+"|"+dir)
	if rel == "results/konflikt.pptx" {
		return ncsync.Root() + "/bericht/konflikt (Helfer).pptx", nil
	}
	if dir == "" {
		dir = ncsync.Root() + "/" + project
	}
	return dir + "/" + rel[strings.LastIndex(rel, "/")+1:], nil
}

func (f *fakeSync) Search(_ context.Context, uid, dir, term string, limit int) ([]ncsync.Entry, error) {
	f.calls = append(f.calls, "suchen|"+dir+"|"+term)
	return []ncsync.Entry{{Path: "Team/Leihvertrag 2027.pdf", Size: 4096, Modified: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}, {Path: "Team/Leihverträge", Dir: true}}, nil
}

func (f *fakeSync) Pull(_ context.Context, uid, project, only string) (ncsync.PullResult, error) {
	f.calls = append(f.calls, "pull|"+project+"|"+only)
	if project == "frei" {
		return ncsync.PullResult{}, ncsync.ErrNotLinked
	}
	return ncsync.PullResult{Updated: []string{"results/folien.pptx"}, Conflicts: []string{"inbox/text (Nextcloud).docx"}}, nil
}

func TestNextcloudSyncTools(t *testing.T) {
	base, _ := url.Parse("https://cloud.example")
	fs := &fakeSync{}
	r := &rec{}
	s := New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: r,
		Log: slog.New(slog.DiscardHandler), Nextcloud: &Nextcloud{Base: base, Files: &ncFiles{got: map[string]string{}}, Sync: fs,
			Creds: func(context.Context, string) (string, string, string, error) { return "z", "z", "pw", nil }}})
	const me = "10.90.1.10:40000"
	if w := rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); !strings.Contains(w.Body.String(), `"nextcloud_upload"`) ||
		!strings.Contains(w.Body.String(), `"nextcloud_pull"`) {
		t.Fatalf("tools/list: %s", w.Body)
	}
	call := func(name, args string) toolResult {
		w := rpc(s, me, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`,"_meta":{"ai.opencode/sessionID":"ses_abcdefgh12"}}}`)
		var resp struct{ Result toolResult }
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Result
	}

	// into the project area without asking
	for _, args := range []string{`{"file":"results/folien.pptx"}`, `{"file":"/workspace/bericht/results/folien.pptx","folder":"Agent Workspace/bericht/Fassungen"}`} {
		if res := call("nextcloud_upload", args); res.IsError || !strings.Contains(res.Content[0].Text, "In Nextcloud abgelegt: Agent Workspace/bericht") {
			t.Fatalf("%s: %+v", args, res)
		}
	}
	// anywhere else refused, no exception (D58): traversal, absolute, encoded,
	// neighbour names; refused without any upstream or sync call
	n := len(fs.calls)
	for _, o := range []string{"/Agent Workspace/../Team", "Team/Presse", "/Team", "Agent Workspace-x/y", "Agent Workspacex",
		"Agent Workspace/%2e%2e/Team", "Agent Workspace/%2E%2E", "Agent Workspace/.project", `Agent Workspace\\..\\Team`, "Agent Workspace/a\\u0000b", "../Agent Workspace"} {
		args := `{"file":"results/a.pdf","folder":"` + o + `","confirmed":true}`
		res := call("nextcloud_upload", args)
		if !res.IsError || !strings.Contains(res.Content[0].Text, "nur in den Ordner „Agent Workspace“") {
			t.Errorf("%s: %+v", args, res)
		}
	}
	if len(fs.calls) != n {
		t.Fatalf("refused calls reached the sync: %v", fs.calls[n:])
	}
	if last := r.ev[len(r.ev)-1]; last.Target != "nextcloud.upload" || last.Status != 403 {
		t.Errorf("usage of a refused call %+v", last)
	}
	// the sync's own refusal reads the same
	fs.err = ncsync.ErrOutside
	if res := call("nextcloud_upload", `{"file":"results/a.pdf"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "Andere Ordner kann ich lesen") {
		t.Errorf("sync refusal: %+v", res)
	}
	fs.err = nil
	if res := call("nextcloud_upload", `{"file":"results/konflikt.pptx"}`); !strings.Contains(res.Content[0].Text, "von der Person geändert") {
		t.Fatalf("conflict: %+v", res)
	}
	if res := call("nextcloud_upload", `{}`); !res.IsError {
		t.Fatal("file required")
	}

	res := call("nextcloud_search", `{"term":"leihvertrag","folder":"Team"}`)
	if res.IsError || !strings.Contains(res.Content[0].Text, "Team/Leihvertrag 2027.pdf (4 KB, 2026-10-01 10:00)") || !strings.Contains(res.Content[0].Text, "[Ordner] Team/Leihverträge/") {
		t.Fatalf("suchen: %+v", res)
	}
	if res := call("nextcloud_search", `{"term":"x","folder":"../etc"}`); !res.IsError {
		t.Fatal("bad folder")
	}

	res = call("nextcloud_pull", `{}`)
	if res.IsError || !strings.Contains(res.Content[0].Text, "Aktualisiert mit der Fassung aus Nextcloud: results/folien.pptx") ||
		!strings.Contains(res.Content[0].Text, "inbox/text (Nextcloud).docx") || fs.calls[len(fs.calls)-1] != "pull|bericht|" {
		t.Fatalf("aktualisieren: %+v", res)
	}
	call("nextcloud_pull", `{"file":"/workspace/bericht/results/folien.pptx"}`)
	if fs.calls[len(fs.calls)-1] != "pull|bericht|results/folien.pptx" {
		t.Fatal(fs.calls)
	}
	if res := call("nextcloud_pull", `{"project":"frei"}`); !strings.Contains(res.Content[0].Text, "nicht mit Nextcloud verknüpft") {
		t.Fatalf("unlinked: %+v", res)
	}

	ro := New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: r,
		Log: slog.New(slog.DiscardHandler), Nextcloud: &Nextcloud{Base: base, Files: &ncFiles{}}})
	if w := rpc(ro, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); strings.Contains(w.Body.String(), "nextcloud_upload") {
		t.Error("write tools listed without the sync")
	}
}
