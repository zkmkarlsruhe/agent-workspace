// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// fakeRooms serves synthetic fixtures with the same feed structure as the live paths.
func fakeRooms(t *testing.T) (*Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	file := func(n string) string {
		b, err := os.ReadFile("testdata/rooms/" + n)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		var body string
		switch r.URL.Path {
		case "/api/events":
			if _, err := time.Parse(time.RFC3339, r.URL.Query().Get("from")); err != nil {
				t.Errorf("from: %q", r.URL.RawQuery)
			}
			body = strings.ReplaceAll(file("events.json"), "{{BASE}}", base)
		case "/api/groups":
			body = file("groups.json")
		case "/verwaltung/feeds/index.json":
			body = file("administration_index.json")
		case "/verwaltung/feeds/all.ics", "/bikes/feeds/all.ics":
			// the MRBS path stays /verwaltung/ (instance URL), the instance and fixture are "administration"
			seg := strings.Split(r.URL.Path, "/")[1]
			body = strings.ReplaceAll(file(map[string]string{"verwaltung": "administration", "bikes": "bikes"}[seg]+"_all.ics"), "\n", "\r\n")
		case "/bikes/feeds/index.json":
			body = file("bikes_index.json")
		case "/lounge/feeds/index.json":
			w.WriteHeader(http.StatusForbidden) // not in the whitelist
			return
		default:
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	u := func(p string) Instance { return Instance{Base: mustURL(srv.URL + "/" + p + "/")} }
	admin, bikes, lounge := u("verwaltung"), u("bikes"), u("lounge")
	admin.Area, admin.Prefix, admin.Team = 1, "mrbs_", "team@example.org"
	bikes.Area, bikes.Prefix = 3, "bikes_"
	lounge.Area, lounge.Prefix = 1, "lounge_"
	s := New(Server{SearXNG: mustURL("http://127.0.0.1:1"), Users: users{}, Usage: &rec{}, Log: slog.New(slog.DiscardHandler),
		Rooms: &Rooms{Timeline: mustURL(srv.URL + "/"), Instances: map[string]Instance{"administration": admin, "bikes": bikes, "lounge": lounge}}})
	return s, &hits
}

func structured(t *testing.T, res toolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	b, _ := json.Marshal(res.Structured)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func TestParseICS(t *testing.T) {
	b, _ := os.ReadFile("testdata/rooms/administration_all.ics")
	ics := strings.ReplaceAll(string(b), "\n", "\r\n")
	from := time.Date(2026, 10, 14, 0, 0, 0, 0, instance.Location())
	bs, err := parseICS(ics, from, from.AddDate(0, 0, 36))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range bs {
		got = append(got, x.start.In(instance.Location()).Format("01-02 15:04")+"-"+x.end.In(instance.Location()).Format("15:04")+" "+x.user)
	}
	// 10-14 single (the cancelled one is gone), 10-21 EXDATE, 11-04 moved, 11-18 after the DST change still 10:00
	want := []string{"10-14 10:00-11:30 anna.test", "11-04 13:00-14:00 bert", "11-18 10:00-12:00 bert"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if bs[0].location != "Area A/Room A" || bs[0].title != "Planning meeting" {
		t.Errorf("first: %+v", bs[0])
	}
}

func TestFree(t *testing.T) {
	d := time.Date(2026, 10, 14, 0, 0, 0, 0, instance.Location())
	at := func(h, m int) time.Time { return d.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	busy := []booking{{start: at(6, 0), end: at(8, 0)}, {start: at(10, 0), end: at(11, 30)}, {start: at(11, 0), end: at(11, 40)}, {start: at(11, 50), end: at(12, 0)}}
	var got []string
	for _, f := range freeSlots(busy, d, d.AddDate(0, 0, 1)) {
		got = append(got, f[0].Format("15:04")+"-"+f[1].Format("15:04"))
	}
	// 11:40-11:50 is shorter than minFree
	if want := "08:00-10:00|12:00-22:00"; strings.Join(got, "|") != want {
		t.Errorf("got %q", got)
	}
}

func TestRoomsFree(t *testing.T) {
	s, _ := fakeRooms(t)
	ctx := context.Background()
	res, m := s.roomsFree(ctx, "rooms_free", json.RawMessage(`{"instance":"administration","from":"2026-10-14","room":"Room A"}`))
	text := res.Content[0].Text
	for _, bad := range []string{"Anna", "anna", "Planning", "Bert", "Vertraulich", "Abgesagt"} {
		if strings.Contains(text, bad) {
			t.Errorf("leaks %q:\n%s", bad, text)
		}
	}
	out := structured(t, res)
	if m.status != 200 || len(out["rooms"].([]any)) != 1 {
		t.Fatalf("%d %s", m.status, text)
	}
	r := out["rooms"].([]any)[0].(map[string]any)
	busy, fr := r["busy"].([]any), r["free"].([]any)
	if len(busy) != 1 || busy[0].(map[string]any)["from"] != "Mi 2026-10-14 10:00" || len(fr) != 2 {
		t.Fatalf("%v", r)
	}
	link := fr[1].(map[string]any)["book_url"].(string)
	if !strings.HasSuffix(link, "/verwaltung/edit_entry.php?area=1&day=14&hour=11&minute=30&month=10&room=101&year=2026") {
		t.Errorf("link %s", link)
	}
	if out["team"].(map[string]any)["mail"] != "team@example.org" {
		t.Errorf("team %v", out["team"])
	}

	res, _ = s.roomsFree(ctx, "rooms_free", json.RawMessage(`{"instance":"bikes","from":"2026-10-14"}`))
	if !res.IsError {
		t.Error("bikes via rooms_free")
	}
	res, _ = s.roomsFree(ctx, "rooms_free", json.RawMessage(`{"instance":"administration","from":"2026-10-14","to":"2027-01-14"}`))
	if !res.IsError || !strings.Contains(res.Content[0].Text, "62") {
		t.Errorf("range: %+v", res)
	}
	res, m = s.roomsFree(ctx, "rooms_free", json.RawMessage(`{"instance":"lounge","from":"2026-10-14"}`))
	if !res.IsError || m.status != http.StatusBadGateway {
		t.Errorf("403 upstream: %+v", res)
	}
}

func TestBikesFree(t *testing.T) {
	s, hits := fakeRooms(t)
	res, _ := s.roomsFree(context.Background(), "bikes_free", json.RawMessage(`{"from":"2026-10-15T08:00","to":"2026-10-15T18:00"}`))
	text := res.Content[0].Text
	if strings.Contains(text, "Carla") || strings.Contains(text, "Anna") || strings.Contains(text, "carla") {
		t.Errorf("leaks a name:\n%s", text)
	}
	out := structured(t, res)
	rooms := out["rooms"].([]any)
	if len(rooms) != 2 || rooms[0].(map[string]any)["room"] != "Bike A" {
		t.Fatalf("%s", text)
	}
	gross := rooms[0].(map[string]any)
	if f := gross["free"].([]any); len(f) != 1 || f[0].(map[string]any)["from"] != "Do 2026-10-15 09:00" || f[0].(map[string]any)["to"] != "Do 2026-10-15 18:00" {
		t.Errorf("free %v", f)
	}
	n := hits.Load()
	s.roomsFree(context.Background(), "bikes_free", json.RawMessage(`{"from":"2026-10-15"}`))
	if hits.Load() != n {
		t.Error("feeds not cached")
	}
}

func TestMyBookings(t *testing.T) {
	s, _ := fakeRooms(t)
	me := store.User{UID: "u1", Username: "anna.test"}
	res, m := s.myBookings(withLang(context.Background(), "en"), me, json.RawMessage(`{"from":"2026-10-14","to":"2026-10-20"}`))
	out := structured(t, res)
	bs := out["bookings"].([]any)
	if m.status != 200 || len(bs) != 2 {
		t.Fatalf("%s", res.Content[0].Text)
	}
	first, second := bs[0].(map[string]any), bs[1].(map[string]any)
	if first["title"] != "Planning meeting" || first["from"] != "Wed 2026-10-14 10:00" || !strings.HasSuffix(first["view_url"].(string), "/verwaltung/view_entry.php?id=11") || first["team"] != "team@example.org" {
		t.Errorf("first %v", first)
	}
	// not in the timeline yet: day view; username case-insensitive
	if second["room"] != "Bike B" || !strings.HasSuffix(second["view_url"].(string), "/bikes/index.php?area=3&day=15&month=10&room=202&view=day&year=2026") {
		t.Errorf("second %v", second)
	}
	if strings.Contains(res.Content[0].Text, "Carla") || strings.Contains(res.Content[0].Text, "Bert") {
		t.Errorf("other people:\n%s", res.Content[0].Text)
	}
	if nb := out["unreachable"].([]any); len(nb) != 1 || nb[0] != "lounge" {
		t.Errorf("unreachable %v", nb)
	}
	res, _ = s.myBookings(context.Background(), store.User{UID: "u2", Username: "nobody"}, json.RawMessage(`{"from":"2026-10-14","to":"2026-10-20"}`))
	if n := structured(t, res)["count"]; n != float64(0) {
		t.Errorf("nobody: %v", n)
	}
}

func TestHouseEvents(t *testing.T) {
	s, _ := fakeRooms(t)
	res, _ := s.houseEvents(context.Background(), json.RawMessage(`{"from":"2026-10-01","to":"2026-10-31"}`))
	text := res.Content[0].Text
	if strings.Contains(text, "Anna") || strings.Contains(text, "Carla") || strings.Contains(text, "view_entry") {
		t.Errorf("booking titles leak:\n%s", text)
	}
	out := structured(t, res)
	ts := out["events"].([]any)
	if len(ts) != 5 || ts[1].(map[string]any)["group"] != "Feiertage" || ts[0].(map[string]any)["location"] != "Room A" {
		t.Fatalf("%s", text)
	}
	res, _ = s.houseEvents(context.Background(), json.RawMessage(`{"from":"2026-10-01","to":"2026-10-31","group":"räume"}`))
	if n := structured(t, res)["count"]; n != float64(2) {
		t.Errorf("group räume: %v", n)
	}
}

func TestRoomsHostsOnly(t *testing.T) {
	s, _ := fakeRooms(t)
	s.Rooms.init()
	for _, u := range []string{"https://example.org/", "http://169.254.169.254/latest/"} {
		if _, _, err := s.Rooms.get(context.Background(), u); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v", u, err)
		}
	}
	h := workspaceRooms()
	if b := h.Instances["administration"].Base.String(); b != "https://rooms.example.org/verwaltung/" {
		t.Errorf("administration base %s", b)
	}
	h.init()
	if tr := h.http.Transport.(roomsTransport); len(tr.hosts) != 2 || !tr.hosts["rooms.example.org"] || !tr.hosts["bikes.example.org"] {
		t.Errorf("hosts %v", tr.hosts)
	}
}

// TestRoomsLive reads the real feeds (the caller's IP must be in workspace-intern-whitelist):
// OC_ROOMS_IT=<own MRBS uid> go test -run RoomsLive -v ./internal/gateway/mcp/
func TestRoomsLive(t *testing.T) {
	uid := os.Getenv("OC_ROOMS_IT")
	if uid == "" {
		t.Skip("OC_ROOMS_IT not set")
	}
	s := New(Server{SearXNG: mustURL("http://127.0.0.1:1"), Users: users{}, Usage: &rec{}, Log: slog.New(slog.DiscardHandler), Rooms: workspaceRooms()})
	ctx := context.Background()
	from := time.Now().In(instance.Location()).Format("2006-01-02")
	to := time.Now().In(instance.Location()).AddDate(0, 0, 14).Format("2006-01-02")
	args := json.RawMessage(`{"from":"` + from + `","to":"` + to + `","instance":"administration"}`)
	for name, res := range map[string]toolResult{
		"house_events": first(s.houseEvents(ctx, args)), "rooms_free": first(s.roomsFree(ctx, "rooms_free", args)),
		"bikes_free": first(s.roomsFree(ctx, "bikes_free", args)), "my_bookings": first(s.myBookings(ctx, store.User{Username: uid}, args)),
	} {
		if res.IsError || strings.Contains(res.Content[0].Text, "mailto:") {
			t.Errorf("%s: %s", name, res.Content[0].Text)
		}
		t.Logf("%s: %d bytes\n%.1500s", name, len(res.Content[0].Text), res.Content[0].Text)
	}
}

func workspaceRooms() *Rooms {
	return HouseRooms(mustURL("https://rooms.example.org/"), mustURL("https://bikes.example.org/"))
}

func first(r toolResult, _ callMeta) toolResult { return r }
