// SPDX-License-Identifier: MIT

package ncsync

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// TestLiveNextcloud runs the sync against a real Nextcloud with an app password:
// OC_NC_LIVE_URL=https://cloud.example.org OC_NC_LIVE_USER=<login> OC_NC_LIVE_PWFILE=<file with the app password>
// It works in "Agent Workspace/harness-<time>/" and removes that folder (and an
// "Agent Workspace" it created itself) at the end.
func TestLiveNextcloud(t *testing.T) {
	raw := os.Getenv("OC_NC_LIVE_URL")
	if raw == "" {
		t.Skip("OC_NC_LIVE_URL not set")
	}
	pw, err := os.ReadFile(os.Getenv("OC_NC_LIVE_PWFILE"))
	if err != nil {
		t.Fatal(err)
	}
	c := Creds{User: os.Getenv("OC_NC_LIVE_USER"), Login: os.Getenv("OC_NC_LIVE_USER"), Password: strings.TrimSpace(string(pw))}
	base, _ := url.Parse(raw)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.EnsureUser(ctx, identity.Identity{UID: uid, Username: c.User})
	project := fmt.Sprintf("harness-%d", time.Now().Unix())
	if err := st.CreateProject(ctx, store.Project{ID: "p1", UID: uid, Folder: project, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(base)
	files := newFakeFiles(time.Now)
	s := &Service{DAV: dav, Store: st, Files: files, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Creds:     func(context.Context, string) (Creds, error) { return c, nil },
		Consented: func(context.Context, string) (bool, error) { return true, nil }}

	_, rootErr := dav.List(ctx, c, Root(), "0")
	del := func(p string) {
		req, _ := http.NewRequest("DELETE", dav.filesURL(c.User, p), nil)
		req.SetBasicAuth(c.Login, c.Password)
		if resp, err := dav.HTTP.Do(req); err == nil {
			resp.Body.Close()
			t.Logf("cleanup DELETE %s: %d", p, resp.StatusCode)
		}
	}
	renamed := project + "-umbenannt"
	defer func() {
		del(Folder(project))
		del(Folder(renamed))
		if HTTPStatus(rootErr) == 404 {
			del(Root())
		}
	}()

	if err := s.Link(ctx, uid, project, false); err != nil {
		t.Fatal(err)
	}
	go s.Loop(ctx)
	wait := func(what string, cond func() bool) time.Duration {
		t.Helper()
		start := time.Now()
		for time.Since(start) < 30*time.Second {
			if cond() {
				return time.Since(start)
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("%s: not within 30 s", what)
		return 0
	}
	get := func(p string) string {
		rc, _, err := dav.Get(ctx, c, p)
		if err != nil {
			return ""
		}
		defer rc.Close()
		var b strings.Builder
		buf := make([]byte, 1<<16)
		for {
			n, err := rc.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				return b.String()
			}
		}
	}

	// the helper writes, the event stream would kick: the file appears in Nextcloud
	files.write(project, "results/folien.txt", "Fassung 1 vom Helfer", time.Now())
	s.Kick(uid)
	d := wait("first push", func() bool { return get(Folder(project)+"/folien.txt") == "Fassung 1 vom Helfer" })
	t.Logf("write in results/ -> visible in Nextcloud after %v", d.Round(100*time.Millisecond))

	files.write(project, "results/folien.txt", "Fassung 2 vom Helfer", time.Now())
	s.Kick(uid)
	d = wait("update in place", func() bool { return get(Folder(project)+"/folien.txt") == "Fassung 2 vom Helfer" })
	t.Logf("update in place visible after %v", d.Round(100*time.Millisecond))

	// the person edits in Nextcloud; the helper writes again: both kept
	es, err := dav.List(ctx, c, Folder(project)+"/folien.txt", "0")
	if err != nil || len(es) != 1 {
		t.Fatal(es, err)
	}
	if _, err := dav.Put(ctx, c, Folder(project)+"/folien.txt", strings.NewReader("Fassung der Person"), es[0].ETag, time.Time{}); err != nil {
		t.Fatal(err)
	}
	files.write(project, "results/folien.txt", "Fassung 3 vom Helfer", time.Now())
	s.Kick(uid)
	wait("conflict copy", func() bool { return get(Folder(project)+"/folien (Helfer).txt") == "Fassung 3 vom Helfer" })
	if got := get(Folder(project) + "/folien.txt"); got != "Fassung der Person" {
		t.Fatalf("the person's edit was overwritten: %q", got)
	}

	// pull back: the project gets the person's version in place (the helper's stays in Nextcloud as the copy)
	res, err := s.Pull(ctx, uid, project, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pull: %+v", res)
	if got, _ := files.read(project, "results/folien.txt"); got != "Fassung der Person" || len(res.Updated) != 1 {
		t.Fatalf("pulled: %q %+v", got, res)
	}
	// the person adds a file in Nextcloud: it comes into results/
	if _, err := dav.Put(ctx, c, Folder(project)+"/notiz.txt", strings.NewReader("von der Person"), "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Pull(ctx, uid, project, ""); err != nil || len(res.New) != 1 {
		t.Fatalf("new file: %+v %v", res, err)
	}
	if got, _ := files.read(project, "results/notiz.txt"); got != "von der Person" {
		t.Fatalf("new file content %q", got)
	}

	// rename follows
	st.RenameProject(ctx, "p1", renamed)
	files.rename(project, renamed)
	s.Kick(uid)
	wait("rename", func() bool { return get(Folder(renamed)+"/folien.txt") != "" })

	hits, err := s.Search(ctx, uid, Root(), "folien", 10)
	t.Logf("search: %d hits, err %v", len(hits), err)
	if err != nil || len(hits) == 0 {
		t.Errorf("search: %v %v", hits, err)
	}
}
