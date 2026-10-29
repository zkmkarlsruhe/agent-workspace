// SPDX-License-Identifier: MIT

package settings

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// Record is what the manager DB keeps per person.
type Record struct {
	Facts    Facts
	Answers  *Answers // nil = form never saved, Default applies
	Text     string   // edited "So arbeite ich"; "" = template of the answers
	Modified time.Time
	Lang     string // portal language ("en" = answers in English unless the form says otherwise)
}

// Store persists a Record (internal/store, migration 8). Load of an
// unknown uid returns the zero Record and no error.
type Store interface {
	LoadSettings(ctx context.Context, uid string) (Record, error)
	SaveFacts(ctx context.Context, uid string, f Facts) error
	SaveSettings(ctx context.Context, uid string, a Answers, text string, at time.Time) error
}

// Directory looks a person up in the Workspace directory.
type Directory interface {
	Lookup(ctx context.Context, email, username string) (Entry, error)
}

// Shell runs a script as uid 1000 in the person's running container
// (lifecycle.Service.Shell); it fails when the container is not running.
type Shell func(ctx context.Context, uid, script string, stdout io.Writer, args ...string) error

// Person is the Authentik identity of a request.
type Person struct {
	UID, Username, Email, Name string
	Groups                     []string
}

// Service keeps the facts fresh (at most daily) and writes the managed block.
type Service struct {
	Store Store
	Dir   Directory // nil = facts only from Authentik
	// Complete calls the fast model on behalf of uid (metered); nil = template only.
	Complete func(ctx context.Context, uid, system, user string) (string, error)
	Shell    Shell
	Log      *slog.Logger
	// Hidden groups are left out of the facts (the access group says nothing about the person).
	Hidden []string

	now     func() time.Time
	mu      sync.Mutex
	checked map[string]time.Time // last Refresh per uid, so requests cost no DB read
}

// ErrNotRunning is what Shell returns for a stopped container.
var ErrNotRunning = errors.New("profil: container not running")

// seenEvery spaces the background checks; Refresh itself decides by MaxAge.
const seenEvery = time.Hour

// MaxAge is how old the directory facts may get before a request refreshes them.
const MaxAge = 24 * time.Hour

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Seen is called on every portal request: at most hourly per person it
// refreshes stale facts in the background.
func (s *Service) Seen(p Person) {
	s.mu.Lock()
	if s.checked == nil {
		s.checked = map[string]time.Time{}
	}
	last, ok := s.checked[p.UID]
	due := !ok || s.clock().Sub(last) >= seenEvery
	if due {
		s.checked[p.UID] = s.clock()
	}
	s.mu.Unlock()
	if due {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := s.Refresh(ctx, p); err != nil {
				if strings.Contains(err.Error(), "FOREIGN KEY") {
					// no account row yet (before consent / first start): try again on the next request
					s.mu.Lock()
					delete(s.checked, p.UID)
					s.mu.Unlock()
					return
				}
				s.Log.WarnContext(ctx, "profil: refresh", "err", err)
			}
		}()
	}
}

// Refresh returns the stored facts, renewed from Authentik and the directory
// when they are older than MaxAge or the Authentik values changed. A new
// state is written to the running container right away.
func (s *Service) Refresh(ctx context.Context, p Person) (Record, error) {
	d, err := s.Store.LoadSettings(ctx, p.UID)
	if err != nil {
		return d, err
	}
	groups := slices.DeleteFunc(slices.Clone(p.Groups), func(g string) bool { return slices.Contains(s.Hidden, g) })
	f := d.Facts
	if f.Name == p.Name && f.Email == p.Email && slices.Equal(f.Groups, groups) && s.clock().Sub(f.AsOf) < MaxAge {
		return d, nil
	}
	nf := Facts{Name: p.Name, Email: p.Email, Groups: groups, AsOf: s.clock(),
		Department: f.Department, Role: f.Role, Phone: f.Phone, Room: f.Room}
	if s.Dir != nil {
		e, err := s.Dir.Lookup(ctx, p.Email, p.Username)
		switch {
		case err == nil:
			nf.Department, nf.Role, nf.Phone, nf.Room = e.Department, e.Role, e.Phone, e.Room
		case errors.Is(err, ErrUnknown):
			nf.Department, nf.Role, nf.Phone, nf.Room = "", "", "", ""
		default:
			// keep the last known directory values; try again in an hour, not tomorrow
			s.Log.WarnContext(ctx, "profil: directory lookup", "err", err)
			nf.AsOf = s.clock().Add(time.Hour - MaxAge)
		}
	}
	if err := s.Store.SaveFacts(ctx, p.UID, nf); err != nil {
		return d, err
	}
	changed := nf.Department != f.Department || nf.Role != f.Role || nf.Phone != f.Phone || nf.Room != f.Room ||
		nf.Name != f.Name || nf.Email != f.Email || !slices.Equal(nf.Groups, f.Groups)
	d.Facts = nf
	if changed {
		if err := s.apply(ctx, p.UID, d); err != nil {
			s.Log.WarnContext(ctx, "profil: apply after refresh", "err", err)
		}
	}
	return d, nil
}

// Current returns the saved answers or the defaults for the facts; someone
// who uses the portal in English gets English answers by default.
func (d Record) Current() Answers {
	if d.Answers != nil {
		return *d.Answers
	}
	a := Default(d.Facts)
	if d.Lang == "en" {
		a.Language = LanguageEnglish
	}
	return a
}

// HowIWork is the "So arbeite ich" text in effect: the edited text, or
// the template in d.Lang. A saved text that is still the untouched template
// of either language counts as the template, so a language switch rewrites it.
func (d Record) HowIWork() string {
	a := d.Current()
	if d.Text == "" || d.Text == Template(a, "de") || d.Text == Template(a, LangEN) {
		return Template(a, d.Lang)
	}
	return d.Text
}

// Block is the managed block for this Record, in its portal language.
func (d Record) Block() string { return Block(d.Facts, d.Current(), d.HowIWork(), d.Lang) }

// Save stores answers and text, then writes the block into the running
// container. ErrTooLarge: nothing stored, the file would exceed 8 KiB.
func (s *Service) Save(ctx context.Context, uid string, a Answers, text string) error {
	d, err := s.Store.LoadSettings(ctx, uid)
	if err != nil {
		return err
	}
	d.Answers, d.Text = &a, text
	if len(d.Block()) > MaxAgents/2 {
		return ErrTooLarge // leaves half of the file to "Gemerkt"
	}
	if err := s.Store.SaveSettings(ctx, uid, a, text, s.clock()); err != nil {
		return err
	}
	return s.apply(ctx, uid, d)
}

// Suggest turns answers into the "So arbeite ich" text (WorkingStyle), in
// the person's portal language.
func (s *Service) Suggest(ctx context.Context, uid string, a Answers) (string, bool) {
	d, err := s.Store.LoadSettings(ctx, uid)
	if err != nil {
		s.Log.WarnContext(ctx, "profil: vorschlag", "err", err) // German then; the text still works
	}
	var c Complete
	if s.Complete != nil {
		c = func(ctx context.Context, system, user string) (string, error) {
			return s.Complete(ctx, uid, system, user)
		}
	}
	return WorkingStyle(ctx, c, a, d.Lang)
}

// AfterStart writes the block into a freshly started container (lifecycle
// hook); also called after a language switch, which changes the block.
func (s *Service) AfterStart(ctx context.Context, uid string) {
	d, err := s.Store.LoadSettings(ctx, uid)
	if err == nil {
		err = s.apply(ctx, uid, d)
	}
	if err != nil {
		s.Log.WarnContext(ctx, "profil: apply after start", "err", err)
	}
}

// readAgents refuses anything but a regular file and reads at most one byte
// past MaxAgents: the agent can point AGENTS.md at /dev/zero or a FIFO.
const readAgents = `[ -e "$1" ] || exit 0; [ -f "$1" ] || { echo "AGENTS.md ist keine Datei" >&2; exit 1; }; head -c 8193 -- "$1"`

// capped keeps at most n bytes, so a misbehaving exec stream cannot grow the
// manager's buffer; apply refuses the cut text.
type capped struct {
	b *bytes.Buffer
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.n - c.b.Len(); len(p) > room {
		c.b.Write(p[:max(room, 0)])
		return max(room, 0), ErrTooLarge
	}
	return c.b.Write(p)
}

// writeAgents replaces the file by rename, so OpenCode never reads half a file.
const writeAgents = `mkdir -p -- "$(dirname -- "$1")" && printf '%s' "$2" > "$1.profil.tmp" && mv -f -- "$1.profil.tmp" "$1"`

// apply merges the block into AGENTS.md if the container runs; a stopped
// container gets it from AfterStart.
//
// shortcut: read-merge-write is not atomic against the helper writing
// "Gemerkt" in the same second; a lost line is rewritten by /merken. Use a
// flock in the container if that ever shows up.
func (s *Service) apply(ctx context.Context, uid string, d Record) error {
	if s.Shell == nil {
		return nil
	}
	var cur bytes.Buffer
	if err := s.Shell(ctx, uid, readAgents, &capped{&cur, MaxAgents + 1}, AgentsPath); errors.Is(err, ErrNotRunning) {
		return nil // AfterStart writes it
	} else if err != nil {
		return err
	}
	if cur.Len() > MaxAgents {
		return ErrTooLarge // never write back a cut file
	}
	out, err := Merge(cur.String(), d.Block())
	if err != nil || out == cur.String() {
		return err
	}
	return s.Shell(ctx, uid, writeAgents, nil, AgentsPath, out)
}
