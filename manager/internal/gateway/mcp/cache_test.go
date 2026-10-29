// SPDX-License-Identifier: MIT

package mcp

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSeenURLsSweep(t *testing.T) {
	s := New(Server{}).seen
	s.add("anna", "old", "fresh")
	s.add("other", "old")
	expired := time.Now().Add(-seenTTL - time.Second)
	s.m["anna"]["old"], s.m["other"]["old"] = expired, expired
	last := s.lastSweep["anna"]
	s.add("anna", "before-hour")
	if _, ok := s.m["anna"]["old"]; !ok || s.lastSweep["anna"] != last {
		t.Fatal("swept before the hourly interval")
	}
	s.lastSweep["anna"] = time.Now().Add(-time.Hour - time.Second)
	s.add("anna", "after-hour")
	if _, ok := s.m["anna"]["old"]; ok {
		t.Fatal("expired URL survived the hourly sweep")
	}
	if !s.has("anna", "fresh") || !s.has("anna", "after-hour") || !s.lastSweep["anna"].After(last) {
		t.Fatal("sweep removed fresh URLs or did not record its time")
	}
	if _, ok := s.m["other"]["old"]; !ok {
		t.Fatal("sweep touched another person's set")
	}
}

func TestPageCacheByteCap(t *testing.T) {
	mb := 1
	s := New(Server{PageCacheMB: &mb})
	c := s.pages
	p := &page{Text: strings.Repeat("界", 200000)}
	c.put("old", p, time.Hour)
	c.put("new", p, time.Hour)
	if c.bytes > c.maxBytes || c.bytes != int64(len("new")+len(p.Text)) {
		t.Fatalf("bytes=%d cap=%d", c.bytes, c.maxBytes)
	}
	if _, ok := c.get("old"); ok {
		t.Fatal("oldest entry survived byte eviction")
	}
	if _, ok := c.get("new"); !ok {
		t.Fatal("new entry missing")
	}
	c.put("new", &page{Text: "small"}, time.Hour)
	if c.bytes != 8 {
		t.Fatalf("replacement bytes=%d", c.bytes)
	}
	c.put("huge", &page{Text: strings.Repeat("x", 1<<20)}, time.Hour)
	if _, ok := c.get("huge"); ok || c.bytes != 8 {
		t.Fatalf("oversize entry cached: bytes=%d", c.bytes)
	}
	c.put("expired", p, -time.Second)
	if _, ok := c.get("expired"); ok || c.bytes != 8 {
		t.Fatalf("expired bytes=%d", c.bytes)
	}
	mb = 0
	legacy := New(Server{PageCacheMB: &mb}).pages
	legacy.max = 1
	legacy.put("old", p, time.Hour)
	legacy.put("new", p, time.Hour)
	if _, ok := legacy.get("old"); ok || len(legacy.m) != 1 {
		t.Fatal("count cap disabled with byte cap")
	}
	if New(Server{}).pages.maxBytes != 64<<20 {
		t.Fatal("default page cap is not 64 MiB")
	}
}

func TestSeenURLsLimit(t *testing.T) {
	s := New(Server{}).seen
	s.add("other", "old")
	keys := make([]string, seenMax+2)
	for i := range keys {
		keys[i] = fmt.Sprint(i)
	}
	s.add("anna", keys...)
	if len(s.m["anna"]) != seenMax || s.has("anna", "0") || s.has("anna", "1") || !s.has("anna", keys[len(keys)-1]) {
		t.Fatal("size or oldest eviction wrong")
	}
	s.add("anna", "2") // Refreshing an existing URL must not evict another one.
	if len(s.m["anna"]) != seenMax {
		t.Fatal("refresh changed size")
	}
	s.add("anna", "new")
	if !s.has("anna", "2") || s.has("anna", "3") || !s.has("other", "old") {
		t.Fatal("refresh or tenant isolation wrong")
	}
}
