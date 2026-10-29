// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Chat collection for the evaluation: on reap ticks of idle running
// environments, incremental, never starting a container, bounded in size,
// and once more right before an idle stop.
func TestCollectChats(t *testing.T) {
	e := setupInMemory(t)
	ctx := context.Background()
	clock := time.Now()
	e.s.now = func() time.Time { return clock }
	tick := func() { clock = clock.Add(time.Second); e.s.Reap(ctx) }
	touch := func() { clock = clock.Add(time.Millisecond); e.s.Touch(uid1) }
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "bericht"}); err != nil {
		t.Fatal(err)
	}
	dir := e.s.projectDir("bericht")
	mk := func(id, parent string, updated int64) {
		se := ocapi.Session{ID: id, Title: "Titel " + id, ParentID: parent, Location: ocapi.Location{Directory: dir}}
		se.Time.Created, se.Time.Updated = 1760700000000, float64(updated)
		e.oc.Sessions = append(e.oc.Sessions, se)
	}
	aborted := `{"id":"m5","type":"assistant","time":{"created":1760700070000},"content":[
	 {"type":"tool","id":"t9","name":"bash","state":{"status":"running","input":{"command":"sleep 99"}}}]}`
	mk("ses_00000001", "", 1760700100000)
	mk("ses_sub00000", "ses_00000001", 1760700100000)
	e.oc.Messages = map[string][]string{"ses_00000001": {tUser, tAssistant, aborted}, "ses_sub00000": {tUser}}

	// evaluation off: no transcript is stored, OpenCode is not even asked
	touch()
	tick()
	if _, err := e.st.ChatByID(ctx, "ses_00000001"); err != store.ErrNotFound || e.oc.PageCalls != 0 {
		t.Fatal("collected without the evaluation", err, e.oc.PageCalls)
	}
	e.s.CollectChats = true
	tick()
	c, err := e.st.ChatByID(ctx, "ses_00000001")
	if err != nil {
		t.Fatal(err)
	}
	if c.UID != uid1 || c.Project != "bericht" || c.Messages != 3 || c.ToolCalls != 5 || c.ToolErrors != 1 || c.Aborted != 1 ||
		c.Truncated || !strings.Contains(c.Transcript, "- Werkzeug bash: ls -la (Fehler: kaputt)") ||
		strings.Contains(c.Transcript, "LANGER SEITENTEXT") || strings.Contains(c.Transcript, "GEHEIMER GEDANKE") {
		t.Fatalf("%+v", c)
	}
	if _, err := e.st.ChatByID(ctx, "ses_sub00000"); err != store.ErrNotFound {
		t.Fatal("subagent chats are not stored on their own")
	}

	// nothing happened: no OpenCode calls at all; a request without changes: list only
	pages, lists := e.oc.PageCalls, e.oc.Lists
	tick()
	if e.oc.PageCalls != pages || e.oc.Lists != lists {
		t.Fatalf("idle tick asked OpenCode: pages %d->%d lists %d->%d", pages, e.oc.PageCalls, lists, e.oc.Lists)
	}
	touch()
	tick()
	if e.oc.PageCalls != pages || e.oc.Lists != lists+1 {
		t.Fatalf("unchanged chat refetched: pages %d->%d", pages, e.oc.PageCalls)
	}

	// a running agent defers the collection; the next idle tick catches up without a request
	e.oc.Sessions[0].Time.Updated++
	e.oc.Messages["ses_00000001"] = append(e.oc.Messages["ses_00000001"], tUser)
	e.oc.Active = []string{"ses_00000001"}
	touch()
	tick()
	if e.oc.PageCalls != pages {
		t.Fatal("collected while an agent runs")
	}
	e.oc.Active = nil
	tick()
	if c, _ = e.st.ChatByID(ctx, "ses_00000001"); c.Messages != 4 {
		t.Fatalf("changed chat not collected: %d messages", c.Messages)
	}

	// truncation: head and tail survive, the middle and oversized messages are cut
	long := []string{fmt.Sprintf(`{"id":"big","type":"user","time":{"created":1},"text":%q}`, strings.Repeat("x", 20<<10))}
	for i := range 200 {
		long = append(long, fmt.Sprintf(`{"id":"a%d","type":"assistant","time":{"created":1},"content":[{"type":"text","text":"Antwort %d %s"}]}`,
			i, i, strings.Repeat("y", 1000)))
	}
	mk("ses_long0000", "", 1760700200000)
	e.oc.Messages["ses_long0000"] = long
	touch()
	tick()
	c, err = e.st.ChatByID(ctx, "ses_long0000")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Truncated || len(c.Transcript) > chatHead+chatTail+200 || c.Messages != 201 ||
		!strings.Contains(c.Transcript, "[... Nachricht gekürzt ...]") || !strings.Contains(c.Transcript, "Nachrichten ausgelassen") ||
		!strings.Contains(c.Transcript, "Antwort 0 ") || !strings.Contains(c.Transcript, "Antwort 199 ") {
		t.Fatalf("truncated %v, %d bytes", c.Truncated, len(c.Transcript))
	}

	// right before the idle stop
	mk("ses_last0000", "", 1760700300000)
	e.oc.Messages["ses_last0000"] = []string{tUser}
	clock = clock.Add(time.Hour)
	e.s.Reap(ctx)
	if e.s.Status(uid1).State != StateAbsent {
		t.Fatal("idle container must stop")
	}
	if _, err := e.st.ChatByID(ctx, "ses_last0000"); err != nil {
		t.Fatal("not collected before the idle stop:", err)
	}

	// stopped: never started for collection
	creates := len(e.rt.Specs)
	mk("ses_off00000", "", 1760700400000)
	e.oc.Messages["ses_off00000"] = []string{tUser}
	e.s.collect(ctx, uid1)
	tick()
	if len(e.rt.Specs) != creates {
		t.Fatal("collection started a container")
	}
	if _, err := e.st.ChatByID(ctx, "ses_off00000"); err != store.ErrNotFound {
		t.Fatal("collected from a stopped environment")
	}
}

func TestCollectSkipsInvalidSessionIDs(t *testing.T) {
	e := setupInMemory(t)
	ctx := context.Background()
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "project"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ses_../../other", "ses_valid123"} {
		se := ocapi.Session{ID: id, Location: ocapi.Location{Directory: e.s.projectDir("project")}}
		se.Time.Updated = 1
		e.oc.Sessions = append(e.oc.Sessions, se)
	}
	e.oc.Messages = map[string][]string{"ses_../../other": {}, "ses_valid123": {}}
	c, _ := e.s.Client(uid1)
	n, err := e.s.collectChats(ctx, uid1, c)
	if err != nil || n != 1 || e.oc.PageCalls != 1 {
		t.Fatalf("stored %d, message requests %d: %v", n, e.oc.PageCalls, err)
	}
	if _, err := e.st.ChatByID(ctx, "ses_../../other"); err != store.ErrNotFound {
		t.Fatal("crafted ID stored", err)
	}
	if _, err := e.st.ChatByID(ctx, "ses_valid123"); err != nil {
		t.Fatal("valid ID skipped", err)
	}
}

func TestCollectChatsSessionLimit(t *testing.T) {
	e := setupInMemory(t)
	ctx := context.Background()
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "bericht"}); err != nil {
		t.Fatal(err)
	}
	for i := range chatSessions + 2 {
		se := ocapi.Session{ID: fmt.Sprintf("ses_%08d", i), Title: strings.Repeat("界\n", 300), Location: ocapi.Location{Directory: e.s.projectDir("bericht")}}
		se.Time.Updated = float64(i + 1)
		e.oc.Sessions = append(e.oc.Sessions, se)
	}
	// A newer child must not consume a root session's place.
	child := e.oc.Sessions[0]
	child.ID, child.ParentID, child.Time.Updated = "ses_child0000", "ses_00000000", 10000
	e.oc.Sessions = append(e.oc.Sessions, child)
	c, ok := e.s.Client(uid1)
	if !ok {
		t.Fatal("runtime absent")
	}
	n, err := e.s.collectChats(ctx, uid1, c)
	if err != nil || n != chatSessions {
		t.Fatalf("stored=%d err=%v", n, err)
	}
	for i := range chatSessions + 2 {
		ch, err := e.st.ChatByID(ctx, fmt.Sprintf("ses_%08d", i))
		if i < 2 {
			if err != store.ErrNotFound {
				t.Fatalf("old session stored: %d %v", i, err)
			}
		} else if err != nil || len(ch.Title) > 200 || strings.Contains(ch.Title, "\n") {
			t.Fatalf("session %d: title=%q err=%v", i, ch.Title, err)
		}
	}
	if _, err := e.st.ChatByID(ctx, child.ID); err != store.ErrNotFound {
		t.Fatal("child stored", err)
	}
}
