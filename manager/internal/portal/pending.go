// SPDX-License-Identifier: MIT

package portal

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"sync"
	"time"
)

// pendingChat is a chat that waits for the environment to start. The waiting
// page auto-posts its one-time token; the first post creates the session and
// stores the link, every later post (reload, second tab, no-JS click) gets the
// same link. So a reload never starts a second, paid run.
type pendingChat struct {
	mu      sync.Mutex // held across CreateSession: concurrent posts wait
	uid     string
	project string
	prompt  string
	link    string
	created time.Time
}

const pendingTTL = time.Hour

var tokenRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// shortcut: in memory, a manager restart drops pending chats (the user then
// lands on the project page and clicks "Neuer Chat"); move to the store if that matters.
type pendingStore struct {
	mu sync.Mutex
	m  map[string]*pendingChat
}

func (p *pendingStore) add(uid, project, prompt string, now time.Time) string {
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]*pendingChat{}
	}
	for k, v := range p.m {
		if now.Sub(v.created) > pendingTTL {
			delete(p.m, k)
		}
	}
	p.m[tok] = &pendingChat{uid: uid, project: project, prompt: prompt, created: now}
	return tok
}

// forget drops every pending chat of uid (account deletion: prompts are personal data).
func (p *pendingStore) forget(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.m {
		if v.uid == uid {
			delete(p.m, k)
		}
	}
}

// get returns the pending chat for tok if it belongs to uid and project.
func (p *pendingStore) get(tok, uid, project string, now time.Time) *pendingChat {
	p.mu.Lock()
	defer p.mu.Unlock()
	pc := p.m[tok]
	if pc == nil || pc.uid != uid || pc.project != project || now.Sub(pc.created) > pendingTTL {
		return nil
	}
	return pc
}
