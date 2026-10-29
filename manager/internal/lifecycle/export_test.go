// SPDX-License-Identifier: MIT

package lifecycle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

const tUser = `{"id":"m1","type":"user","time":{"created":1760700000000},"text":"Fasse https://example.com zusammen",
 "files":[{"data":"iVBORw0KGgo=","mime":"image/png","source":{"type":"inline"},"name":"skizze.png"}]}`

const tAssistant = `{"id":"m2","type":"assistant","agent":"build","native":{"replay":"NATIVE_REPLAY"},"time":{"created":1760700060000},"content":[
 {"type":"reasoning","text":"GEHEIMER GEDANKE","native":{"replay":"NATIVE_REPLAY"}},
 {"type":"tool","id":"t1","name":"webfetch","native":{"status":"error","input":{"url":"https://native.invalid"},"replay":"NATIVE_REPLAY"},"state":{"status":"completed","input":{"url":"https://example.com","format":"markdown"},
  "content":[{"type":"text","text":"# Example Domain\nLANGER SEITENTEXT"}]}},
 {"type":"tool","id":"t2","name":"websearch","state":{"status":"completed","input":{"query":"workspace\nexample"},
  "content":[{"type":"text","text":"## [Workspace Startseite](https://example.org/)\nText\n## [Wiki](https://de.wikipedia.org/wiki/Workspace)"}]}},
 {"type":"tool","id":"t3","name":"bash","state":{"status":"error","input":{"command":"ls -la"},"error":{"type":"Tool.Error","message":"kaputt"}}},
 {"type":"tool","id":"t4","name":"task","state":{"status":"completed","input":{"description":"Unterseiten lesen"},"content":[{"type":"text","text":"x"}]}},
 {"type":"text","text":"Die Seite ist ein Beispiel.","native":{"replay":"NATIVE_REPLAY"}}]}`

func TestRenderMessage(t *testing.T) {
	var msgs []ocapi.Message
	for _, raw := range []string{tUser, tAssistant, `{"id":"m3","type":"system","text":"SYSTEMTEXT","time":{"created":1}}`,
		`{"id":"m4","type":"idle","outcome":"succeeded","time":{"created":1}}`} {
		var m ocapi.Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(renderMessage(m, false))
	}
	en := renderMessage(msgs[0], true) + renderMessage(msgs[1], true)
	for _, want := range []string{"## You, 2025-", "Attached: skizze.png", "## Helper, ", "- Tool bash: ls -la (error: kaputt)\n", "Sources:\n"} {
		if !strings.Contains(en, want) {
			t.Errorf("English transcript lacks %q:\n%s", want, en)
		}
	}
	got := b.String()
	for _, want := range []string{
		"## Du, ", "Fasse https://example.com zusammen\n", "Angehängt: skizze.png",
		"## Helfer, ", "Die Seite ist ein Beispiel.",
		"- Werkzeug webfetch: https://example.com (fertig)\n",
		"- Werkzeug websearch: workspace example (fertig)\n",
		"- Werkzeug bash: ls -la (Fehler: kaputt)\n",
		"- Werkzeug task: Unterseiten lesen (fertig), Teilaufgabe an einen weiteren Helfer",
		"Quellen:\n- https://example.com\n- [Workspace Startseite](https://example.org/)\n- [Wiki](https://de.wikipedia.org/wiki/Workspace)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript lacks %q", want)
		}
	}
	for _, not := range []string{"GEHEIMER GEDANKE", "LANGER SEITENTEXT", "SYSTEMTEXT", "iVBORw0KGgo", "NATIVE_REPLAY", "native.invalid"} {
		if strings.Contains(got, not) {
			t.Errorf("transcript contains %q", not)
		}
	}
	if fileSlug("Größe & Umsatz: Q3/2025!") != "groesse-umsatz-q3-2025" || fileSlug("???") != "chat" {
		t.Fatal(fileSlug("Größe & Umsatz: Q3/2025!"))
	}
}

func readZip(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		c, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(c)
	}
	return out
}

func TestExport(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, n := range []string{"bericht", "alt"} {
		if err := e.s.CreateProject(ctx, uid1, NewProject{Name: n, Description: "d"}); err != nil {
			t.Fatal(err)
		}
	}
	ws := e.s.ws
	os.WriteFile(filepath.Join(ws, "bericht", "results", "bericht.md"), []byte("# Bericht"), 0o644)
	os.MkdirAll(filepath.Join(ws, "bericht", "work", ".cache"), 0o755)
	os.WriteFile(filepath.Join(ws, "bericht", "work", ".cache", "x"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(ws, "bericht", "work", ".env"), []byte("SECRET"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(ws, "bericht", "inbox", "pw"))
	os.WriteFile(filepath.Join(ws, "alt", "inbox", "a.txt"), []byte("alt"), 0o644)

	mk := func(id, title, dir, parent string, created time.Time) {
		se := ocapi.Session{ID: id, Title: title, ParentID: parent, Location: ocapi.Location{Directory: dir}}
		se.Time.Created = float64(created.UnixMilli())
		se.Time.Updated = se.Time.Created
		e.oc.Sessions = append(e.oc.Sessions, se)
	}
	day := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	mk("ses_1", "Bericht schreiben", e.s.projectDir("bericht"), "", day)
	mk("ses_2", "Bericht schreiben", e.s.projectDir("bericht"), "", day.Add(time.Hour)) // same title, same day
	mk("ses_sub", "Teilaufgabe", e.s.projectDir("bericht"), "ses_1", day)
	mk("ses_alt", "Alter Chat", e.s.projectDir("alt"), "", day)
	// a long chat: 45 messages, more than two pages
	long := []string{tUser}
	for i := range 44 {
		long = append(long, fmt.Sprintf(`{"id":"a%d","type":"assistant","time":{"created":1760700060000},"content":[{"type":"text","text":"Antwort %d"}]}`, i, i))
	}
	e.oc.Messages = map[string][]string{"ses_1": long, "ses_2": {tUser}, "ses_alt": {tUser, tAssistant}, "ses_sub": {tUser}}
	if err := e.s.MoveToTrash(ctx, uid1, "alt"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.EnvStop(ctx, uid1); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := e.s.Export(ctx, uid1, "", &buf); err != nil { // starts the environment like other file ops
		t.Fatal(err)
	}
	files := readZip(t, buf.Bytes())
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	tr, _ := e.s.ListTrash(ctx, uid1)
	want := []string{
		"README.txt",
		"bericht/chats/2026-10-01-bericht-schreiben-2.md",
		"bericht/chats/2026-10-01-bericht-schreiben.md",
		"bericht/files/results/bericht.md",
		"trash/alt-" + tr[0].ID + "/chats/2026-10-01-alter-chat.md",
		"trash/alt-" + tr[0].ID + "/files/inbox/a.txt",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("zip entries\n got %q\nwant %q", names, want)
	}
	if c := files["bericht/chats/2026-10-01-bericht-schreiben.md"]; !strings.HasPrefix(c, "# Bericht schreiben\n\n- Projekt: bericht\n") ||
		!strings.Contains(c, "Antwort 0\n") || !strings.Contains(c, "Antwort 43\n") {
		t.Fatalf("long transcript:\n%s", c)
	}
	if c := files["trash/alt-"+tr[0].ID+"/chats/2026-10-01-alter-chat.md"]; !strings.Contains(c, "Quellen:") {
		t.Fatalf("trash transcript:\n%s", c)
	}
	if !strings.Contains(files["README.txt"], "trash/") {
		t.Fatal(files["README.txt"])
	}
	if e.oc.PageCalls < 3+1+1 {
		t.Fatalf("long chat not paged: %d page calls", e.oc.PageCalls)
	}

	// one project: same layout, no trash, no other project
	buf.Reset()
	if err := e.s.Export(ctx, uid1, "bericht", &buf); err != nil {
		t.Fatal(err)
	}
	one := readZip(t, buf.Bytes())
	if len(one) != 4 || one["bericht/files/results/bericht.md"] != "# Bericht" || strings.Contains(one["README.txt"], "trash/") {
		t.Fatalf("%v", one)
	}
	if err := e.s.Export(ctx, uid1, "fehlt", io.Discard); err != ErrNotFound {
		t.Fatal(err)
	}
	if err := e.s.Export(ctx, uid1, "../x", io.Discard); err != ErrInvalid {
		t.Fatal(err)
	}
}

func TestExportAssistant(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "bericht", Description: "d"}); err != nil {
		t.Fatal(err)
	}
	e.s.Settings = func(_ context.Context, uid string) (string, error) {
		return "## Über mich\n- Name: " + uid[:4] + "\n", nil
	}
	var buf bytes.Buffer
	if err := e.s.Export(ctx, uid1, "", &buf); err != nil {
		t.Fatal(err)
	}
	files := readZip(t, buf.Bytes())
	if files["profile.md"] != "## Über mich\n- Name: "+uid1[:4]+"\n" || !strings.Contains(files["README.txt"], "profile.md") {
		t.Fatalf("profile.md %q\n%s", files["profile.md"], files["README.txt"])
	}
	buf.Reset()
	if err := e.s.Export(ctx, uid1, "bericht", &buf); err != nil {
		t.Fatal(err)
	}
	if _, ok := readZip(t, buf.Bytes())["profile.md"]; ok {
		t.Error("project export carries the profile")
	}
}

// The export writes while it reads: the first bytes reach w before the last
// file is read from the container.
func TestExportStreams(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "gross"}); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 4<<20) // random: deflate cannot shrink it
	rand.Read(big)
	for i := range 4 {
		os.WriteFile(filepath.Join(e.s.ws, "gross", "inbox", fmt.Sprintf("f%d.bin", i)), big, 0o644)
	}
	w := &firstWrite{}
	if err := e.s.Export(ctx, uid1, "gross", w); err != nil {
		t.Fatal(err)
	}
	if w.firstAt == 0 || w.firstAt > w.n/4 {
		t.Fatalf("first write after %d of %d bytes", w.firstAt, w.n)
	}
}

type firstWrite struct{ n, firstAt int }

func (f *firstWrite) Write(p []byte) (int, error) {
	if f.firstAt == 0 {
		f.firstAt = len(p)
	}
	f.n += len(p)
	return len(p), nil
}

func TestTranscriptPageLimit(t *testing.T) {
	for _, more := range []bool{false, true} {
		t.Run(fmt.Sprint(more), func(t *testing.T) {
			calls := 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls > maxPages {
					http.Error(w, "page limit exceeded", http.StatusBadRequest)
					return
				}
				next := "repeated"
				if calls == maxPages && !more {
					next = ""
				}
				// Full pages with no visible text isolate page truncation from byte limits.
				json.NewEncoder(w).Encode(map[string]any{"data": make([]ocapi.Message, exportPage), "cursor": map[string]string{"next": next}})
			})
			c := &ocapi.Client{BaseURL: "http://runtime", HTTP: &http.Client{Transport: assistantTransport{handler}}}
			se := ocapi.Session{ID: "ses_00000001"}
			ch, err := snapshot(context.Background(), c, se)
			if err != nil || calls != maxPages || ch.Truncated != more {
				t.Fatalf("snapshot: calls=%d truncated=%v err=%v", calls, ch.Truncated, err)
			}
			calls = 0
			var out strings.Builder
			err = writeTranscript(context.Background(), c, se, "p", &out, true)
			if err != nil || calls != maxPages || strings.Contains(out.String(), "Transcript truncated") != more {
				t.Fatalf("export: calls=%d out=%q err=%v", calls, out.String(), err)
			}
		})
	}
}
