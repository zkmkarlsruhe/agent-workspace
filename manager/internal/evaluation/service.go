// SPDX-License-Identifier: MIT

package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// ErrRunning: an evaluation run is already in progress.
var ErrRunning = errors.New("auswertung: run in progress")

// Bounds of one run and of the transcript the model reads.
const (
	runMax     = 200
	promptHead = 16 << 10
	promptTail = 8 << 10
	listMax    = 500
)

type Service struct {
	Store *store.Store
	// Complete asks the fast model, metered as manager usage (store.ManagerUID);
	// nil = heuristic flags only.
	Complete func(ctx context.Context, system, user string) (string, error)
	Log      *slog.Logger
	Now      func() time.Time
	DayStart func(time.Time) time.Time // start of the configured day (config.DayStart)

	mu      sync.Mutex // held during a run
	stateMu sync.Mutex
	running bool
	last    RunResult
	autoDay time.Time // day of the last automatic run, Loop only
}

// RunResult describes the last finished run.
type RunResult struct {
	At             time.Time
	LLM, Heuristic int
	Err            string
}

// State reports whether a run is in progress and the last result.
func (s *Service) State() (running bool, last RunResult) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.running, s.last
}

// Start runs Run in the background (the button in /admin); ErrRunning if one runs.
func (s *Service) Start() error {
	if !s.mu.TryLock() {
		return ErrRunning
	}
	go func() {
		defer s.mu.Unlock()
		s.run(context.Background())
	}()
	return nil
}

// Run evaluates every chat without evaluation or changed since its last one,
// at most runMax oldest changes per run.
func (s *Service) Run(ctx context.Context) (RunResult, error) {
	if !s.mu.TryLock() {
		return RunResult{}, ErrRunning
	}
	defer s.mu.Unlock()
	return s.run(ctx)
}

func (s *Service) run(ctx context.Context) (RunResult, error) {
	s.stateMu.Lock()
	s.running = true
	s.stateMu.Unlock()
	res, err := s.evaluate(ctx)
	res.At = s.Now()
	if err != nil {
		res.Err = err.Error()
		s.Log.ErrorContext(ctx, "evaluation run failed", "llm", res.LLM, "heuristic", res.Heuristic, "err", err)
	} else {
		s.Log.InfoContext(ctx, "evaluation run", "llm", res.LLM, "heuristic", res.Heuristic)
	}
	s.stateMu.Lock()
	s.running, s.last = false, res
	s.stateMu.Unlock()
	return res, err
}

func (s *Service) evaluate(ctx context.Context) (RunResult, error) {
	var res RunResult
	chats, err := s.Store.ChatsToEvaluate(ctx, consent.Version(), runMax)
	if err != nil {
		return res, err
	}
	s.Log.DebugContext(ctx, "evaluation: chats to evaluate", "n", len(chats), "max", runMax)
	for _, c := range chats {
		ctx := logx.With(ctx, logx.Fields{UID: c.UID, SessionID: c.SessionID})
		// the list is up to runMax model calls old: a deletion or a withdrawn
		// consent since then must keep the transcript away from the model
		ok, err := s.Store.MayEvaluateChat(ctx, c.UID, c.SessionID, consent.Version())
		if err != nil {
			return res, err
		}
		if !ok {
			s.Log.DebugContext(ctx, "evaluation: chat skipped", "reason", "deleted or consent withdrawn")
			continue
		}
		t0 := time.Now()
		e, src := s.judge(ctx, c.Chat)
		s.Log.DebugContext(ctx, "evaluation: chat judged", "source", src, "transcript_bytes", len(c.Transcript), "duration_ms", time.Since(t0).Milliseconds())
		b, _ := json.Marshal(e)
		stored, err := s.Store.PutEval(ctx, c.UID, c.SessionID, consent.Version(), store.ChatEval{ChatUpdated: c.Updated, EvaluatedAt: s.Now(), Source: src, Result: string(b)})
		if err != nil {
			return res, err
		}
		if !stored {
			s.Log.DebugContext(ctx, "evaluation: result dropped", "reason", "chat changed, deleted or consent withdrawn")
			continue
		}
		if src == "llm" {
			res.LLM++
		} else {
			res.Heuristic++
		}
	}
	return res, nil
}

// judge asks the model and falls back to the heuristic on any failure; the
// measured problems are always part of the verdict.
func (s *Service) judge(ctx context.Context, c store.Chat) (Eval, string) {
	if s.Complete == nil {
		return Heuristic(c), "heuristic"
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	answer, err := s.Complete(cctx, instance.Text(systemPrompt), prompt(c))
	if err == nil {
		var e Eval
		if e, err = Parse(answer); err == nil {
			e.Problems = measured(c, e.Problems)
			return e, "llm"
		}
	}
	s.Log.WarnContext(ctx, "chat evaluation fell back to heuristics", "session", c.SessionID, "err", err)
	return Heuristic(c), "heuristic"
}

func prompt(c store.Chat) string {
	t := c.Transcript
	if len(t) > promptHead+promptTail {
		h, tl := promptHead, len(t)-promptTail
		for h > 0 && !utf8.RuneStart(t[h]) {
			h--
		}
		for tl < len(t) && !utf8.RuneStart(t[tl]) {
			tl++
		}
		t = t[:h] + "\n[... ausgelassen ...]\n" + t[tl:]
	}
	return fmt.Sprintf("Titel: %s\nWerkzeugaufrufe: %d, davon Fehler: %d\n\n%s", c.Title, c.ToolCalls, c.ToolErrors, t)
}

// Loop runs the evaluation once a day after 03:00 in the configured timezone (and right
// after a restart past that hour: a second run only touches changed chats).
func (s *Service) Loop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		now := s.Now()
		if day := s.DayStart(now); now.Sub(day) >= 3*time.Hour && !day.Equal(s.autoDay) {
			if _, err := s.Run(ctx); !errors.Is(err, ErrRunning) {
				s.autoDay = day
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Day is one row of the overview.
type Day struct {
	Day time.Time
	store.DayStat
}

// Overview returns the last n Berlin days, newest first.
func (s *Service) Overview(ctx context.Context, n int) ([]Day, error) {
	var out []Day
	day := s.DayStart(s.Now())
	for range n {
		d, err := s.Store.DayStat(ctx, day, s.DayStart(day.Add(36*time.Hour)))
		if err != nil {
			return nil, err
		}
		out = append(out, Day{day, d})
		day = s.DayStart(day.Add(-12 * time.Hour))
	}
	return out, nil
}

// Item is one chat in the admin views.
type Item struct {
	store.ChatRow
	E     *Eval // nil = not evaluated in its current state
	Stale bool  // evaluated, but the chat changed since
	Flags []string
}

func item(c store.ChatRow) Item {
	it := Item{ChatRow: c}
	if c.Eval != nil {
		var e Eval
		if json.Unmarshal([]byte(c.Eval.Result), &e) == nil {
			it.E = &e
		}
		it.Stale = c.Eval.ChatUpdated.Before(c.Updated)
	}
	it.Flags = Flags(c.Chat, it.E)
	return it
}

// Filter of the chat list: last activity in [From, To).
type Filter struct{ From, To time.Time }

type ChatList struct {
	Items     []Item
	Truncated bool
}

// Chats lists at most listMax chats of the period, newest first.
func (s *Service) Chats(ctx context.Context, f Filter) (ChatList, error) {
	rows, err := s.Store.Chats(ctx, f.From, f.To, listMax)
	if err != nil {
		return ChatList{}, err
	}
	out := make([]Item, len(rows))
	for i, r := range rows {
		out[i] = item(r)
	}
	return ChatList{Items: out, Truncated: len(rows) == listMax}, nil
}

// Reader is the admin who opens a chat.
type Reader struct{ UID, Username string }

// Chat returns one chat with transcript and records the read: log line and
// admin_audit row, before anything is shown.
func (s *Service) Chat(ctx context.Context, r Reader, id string) (Item, error) {
	c, err := s.Store.ChatByID(ctx, id)
	if err != nil {
		return Item{}, err
	}
	if err := s.Store.AuditChatRead(ctx, s.Now(), r.UID, r.Username, id); err != nil {
		return Item{}, err
	}
	s.Log.InfoContext(ctx, "admin chat read", "admin", r.Username, "admin_uid", r.UID, "session", id, "person_uid", c.UID)
	return item(c), nil
}

// Audit returns the newest read records.
func (s *Service) Audit(ctx context.Context, n int) ([]store.AuditEntry, error) {
	return s.Store.Audit(ctx, n)
}
