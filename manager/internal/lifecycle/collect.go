// SPDX-License-Identifier: MIT

package lifecycle

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Bounds of one stored transcript: the first chatHead bytes, then the last
// chatTail bytes; one message counts at most chatMsg bytes. Tool outputs are
// never stored, only the one-line summary of the export.
const (
	chatHead     = 48 << 10
	chatTail     = 16 << 10
	chatMsg      = 8 << 10
	chatSessions = 500
)

// collectIfIdle snapshots the chats of a running environment when something
// may have changed since the last collection (a request came in, or an agent
// was busy at the last look) and no agent runs now. A busy person lock (start,
// stop, account deletion) skips the tick.
func (s *Service) collectIfIdle(ctx context.Context, uid string, r running) {
	if !s.CollectChats {
		return
	}
	if !r.lastRequest.After(r.collected) && !r.busySeen {
		return
	}
	l := s.lock(uid)
	if !l.TryLock() {
		return
	}
	defer l.Unlock()
	c, ok := s.Client(uid)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	active, err := c.Active(cctx)
	cancel()
	if err != nil {
		return
	}
	busy := len(active) > 0
	if !busy {
		s.collect(ctx, uid)
	}
	s.mu.Lock()
	if cur := s.run[uid]; cur != nil {
		cur.busySeen = busy
		if !busy {
			cur.collected = s.now()
		}
	}
	s.mu.Unlock()
}

// collect stores a snapshot of every new or changed root chat of uid's
// projects (store.Chat) for the evaluation. Only a running environment is
// asked; the container is never started for it. Errors are logged and end
// the collection; the next call retries what is still unchanged in the store.
// Without the evaluation (CollectChats false) nothing is stored.
func (s *Service) collect(ctx context.Context, uid string) {
	if !s.CollectChats {
		return
	}
	c, ok := s.Client(uid)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	n, err := s.collectChats(ctx, uid, c)
	if err != nil {
		s.log.WarnContext(ctx, "chat collection failed", "uid", uid, "stored", n, "err", err)
		return
	}
	if n > 0 {
		s.log.InfoContext(ctx, "chats collected", "uid", uid, "stored", n)
	}
}

func (s *Service) collectChats(ctx context.Context, uid string, c *ocapi.Client) (int, error) {
	known, err := s.st.ChatUpdates(ctx, uid)
	if err != nil {
		return 0, err
	}
	ps, err := s.st.Projects(ctx, uid)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range ps {
		if p.Assistant.ID != "" {
			continue
		}
		sess, err := c.SessionListAll(ctx, s.projectDir(p.Folder))
		if err != nil {
			return n, err
		}
		sess = slices.DeleteFunc(sess, func(se ocapi.Session) bool { return !store.ValidSessionID(se.ID) || se.ParentID != "" })
		slices.SortStableFunc(sess, func(a, b ocapi.Session) int { return cmp.Compare(b.Time.Updated, a.Time.Updated) })
		if len(sess) > chatSessions {
			sess = sess[:chatSessions]
		}
		for _, se := range sess {
			if int64(se.Time.Updated) == known[se.ID] {
				continue
			}
			ch, err := snapshot(ctx, c, se)
			if err != nil {
				return n, err
			}
			ch.UID, ch.Project, ch.CollectedAt = uid, p.Folder, s.now()
			if err := s.st.PutChat(ctx, ch); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// snapshot renders one chat page by page like the export and counts tool
// calls, tool errors, aborts and model errors.
func snapshot(ctx context.Context, c *ocapi.Client, se ocapi.Session) (store.Chat, error) {
	ch := store.Chat{SessionID: se.ID, Title: oneLine(se.Title, 200),
		Created: time.UnixMilli(int64(se.Time.Created)), Updated: time.UnixMilli(int64(se.Time.Updated))}
	if se.Outcome == "interrupted" {
		ch.Aborted++
	}
	var t bounded
	cursor := ""
	for page := 0; page < maxPages; page++ {
		msgs, next, err := c.Messages(ctx, se.ID, cursor, exportPage)
		if err != nil {
			return ch, err
		}
		for _, m := range msgs {
			countMessage(&ch, m)
			t.add(renderMessage(m, false)) // the evaluation reads German transcripts
		}
		if next == "" {
			cursor = ""
			break
		}
		cursor = next
	}
	ch.Transcript, ch.Truncated = t.String()
	ch.Truncated = ch.Truncated || cursor != ""
	return ch, nil
}

func countMessage(ch *store.Chat, m ocapi.Message) {
	switch m.Type {
	case "user", "shell", "assistant":
		ch.Messages++
	}
	for _, p := range m.Content {
		if p.Type != "tool" {
			continue
		}
		ch.ToolCalls++
		switch p.State.Status {
		case "completed":
		case "error":
			ch.ToolErrors++
		default:
			ch.Aborted++
		}
	}
	if msg := errMessage(m.Error); msg != "" {
		if strings.Contains(strings.ToLower(string(m.Error)), "abort") {
			ch.Aborted++
		} else {
			ch.ModelErrors++
		}
	}
}

// bounded keeps the head and a rolling tail of the rendered messages.
type bounded struct {
	head      strings.Builder
	tail      []string
	tailLen   int
	full      bool // head is complete; everything else goes to tail
	dropped   int  // messages cut out between head and tail
	shortened bool // a single message was cut
}

func (b *bounded) add(s string) {
	if s == "" {
		return
	}
	if len(s) > chatMsg {
		s, b.shortened = cut(s, chatMsg), true
	}
	if !b.full && b.head.Len()+len(s) <= chatHead {
		b.head.WriteString(s)
		return
	}
	b.full = true
	b.tail = append(b.tail, s)
	b.tailLen += len(s)
	for b.tailLen > chatTail && len(b.tail) > 1 {
		b.tailLen -= len(b.tail[0])
		b.tail = b.tail[1:]
		b.dropped++
	}
}

func (b *bounded) String() (string, bool) {
	out := b.head.String()
	if b.dropped > 0 {
		out += fmt.Sprintf("\n[... %d Nachrichten ausgelassen ...]\n", b.dropped)
	}
	return out + strings.Join(b.tail, ""), b.dropped > 0 || b.shortened
}
