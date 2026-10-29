// SPDX-License-Identifier: MIT

package mcp

import (
	"net/url"
	"strings"
	"sync"
	"time"
)

type entry[V any] struct {
	v     V
	exp   time.Time
	bytes int64
	order uint64
}

// ttlCache drops expired entries, then the oldest insertions to satisfy its caps.
type ttlCache[V any] struct {
	mu       sync.Mutex
	m        map[string]entry[V]
	max      int
	maxBytes int64 // 0 = count only
	bytes    int64
	order    uint64
	size     func(V) int64 // payload bytes, excluding the key
}

func newTTLCache[V any](max int) *ttlCache[V] {
	return &ttlCache[V]{m: map[string]entry[V]{}, max: max}
}

func (c *ttlCache[V]) get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.exp) {
		if ok {
			c.remove(k)
		}
		var zero V
		return zero, false
	}
	return e.v, true
}

func (c *ttlCache[V]) put(k string, v V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remove(k)
	n := int64(len(k))
	if c.size != nil {
		n += c.size(v)
	}
	if c.maxBytes > 0 && n > c.maxBytes {
		return
	}
	c.order++
	now := time.Now()
	c.m[k] = entry[V]{v: v, exp: now.Add(ttl), bytes: n, order: c.order}
	c.bytes += n
	over := func() bool { return len(c.m) > c.max || c.maxBytes > 0 && c.bytes > c.maxBytes }
	if over() {
		for key, e := range c.m {
			if now.After(e.exp) {
				c.remove(key)
			}
		}
	}
	for over() {
		oldest, order := "", ^uint64(0)
		for key, e := range c.m {
			if e.order < order {
				oldest, order = key, e.order
			}
		}
		c.remove(oldest)
	}
}

// remove requires c.mu so replacement and expiry keep the byte total consistent.
func (c *ttlCache[V]) remove(k string) {
	c.bytes -= c.m[k].bytes
	delete(c.m, k)
}

// seenURLs is the per-person list of URLs that appeared in tool output
// (search hits, link lists): provenance rule A5 (a), docs/adr/README.md §3.6.
// shortcut: per person, not per session, RAM only, lost on restart; upgrade with the source log web_quellen (§3.4).
type seenURLs struct {
	mu        sync.Mutex
	m         map[string]map[string]time.Time
	lastSweep map[string]time.Time
}

const seenTTL = 24 * time.Hour
const seenMax = 5000

func (s *seenURLs) add(uid string, keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.m[uid]
	if set == nil {
		set = map[string]time.Time{}
		s.m[uid] = set
	}
	now := time.Now()
	if s.lastSweep == nil {
		s.lastSweep = map[string]time.Time{}
	}
	if now.Sub(s.lastSweep[uid]) >= time.Hour {
		for k, t := range set {
			if now.Sub(t) >= seenTTL {
				delete(set, k)
			}
		}
		s.lastSweep[uid] = now
	}
	for _, k := range keys {
		if _, exists := set[k]; !exists && len(set) >= seenMax {
			oldest := ""
			var at time.Time
			for key, t := range set {
				if at.IsZero() || t.Before(at) {
					oldest, at = key, t
				}
			}
			delete(set, oldest)
		}
		set[k] = time.Now()
	}
}

func (s *seenURLs) has(uid, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[uid][key]
	return ok && time.Since(t) < seenTTL
}

// normURL is the cache and provenance key: lower-case host, no fragment,
// no tracking parameters, sorted query.
func normURL(u *url.URL) string {
	n := *u
	n.Fragment, n.RawFragment = "", ""
	n.Host = strings.ToLower(strings.TrimSuffix(n.Host, "."))
	n.Scheme = strings.ToLower(n.Scheme)
	if n.Path == "" {
		n.Path = "/"
	}
	q := n.Query()
	for k := range q {
		if strings.HasPrefix(k, "utm_") || k == "fbclid" || k == "gclid" {
			q.Del(k)
		}
	}
	n.RawQuery = q.Encode()
	n.ForceQuery = false
	return n.String()
}
