// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/evaluation"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Admin is the evaluation behind /admin (internal/evaluation); nil = no area.
type Admin interface {
	Overview(ctx context.Context, days int) ([]evaluation.Day, error)
	Chats(ctx context.Context, f evaluation.Filter) (evaluation.ChatList, error)
	// Chat records the read (audit) before returning the transcript.
	Chat(ctx context.Context, r evaluation.Reader, id string) (evaluation.Item, error)
	Report(ctx context.Context, day time.Time) (string, error)
	Start() error
	State() (running bool, last evaluation.RunResult)
	Audit(ctx context.Context, n int) ([]store.AuditEntry, error)
}

type IdentityGuard interface {
	IdentityBlocks(context.Context) ([]store.IdentityBlock, error)
	ReleaseIdentity(ctx context.Context, uid, pendingHash, adminUID string) error
}

func init() {
	pageFiles = append(pageFiles, "admin.html", "admin-chats.html", "admin-chat.html", "identity-blocked.html")
}

func (s *Server) isAdmin(id Identity) bool {
	return (s.admin != nil || s.identityGuard != nil) && s.adminGroup != "" && slices.Contains(id.Groups, s.adminGroup)
}

// adminRoutes: everyone outside the admin group gets the plain 404 page, as
// for a path that does not exist.
func (s *Server) adminRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if (s.admin == nil && s.identityGuard == nil) || s.adminGroup == "" {
		return
	}
	a := func(pattern string, h func(http.ResponseWriter, *http.Request, Identity)) {
		page(pattern, func(w http.ResponseWriter, r *http.Request, id Identity) {
			if !s.isAdmin(id) {
				s.errorPage(w, r, http.StatusNotFound, "Page not found", "This page does not exist.")
				return
			}
			h(w, r, id)
		})
	}
	a("GET /admin", s.adminHome)
	if s.admin != nil {
		a("GET /admin/report", s.adminReport)
		a("POST /admin/evaluate", s.adminRun)
		a("GET /admin/chats", s.adminChats)
		a("GET /admin/chats/{id}", s.adminChat)
	}
	if s.identityGuard != nil {
		a("POST /admin/identity/{uid}/release", s.adminIdentityRelease)
	}
	s.feedbackAdminRoutes(a)
}

// day parses ?<key>=JJJJ-MM-TT as a Berlin day; def if missing or invalid.
func (s *Server) day(r *http.Request, key string, def time.Time) time.Time {
	t, err := time.Parse("2006-01-02", r.URL.Query().Get(key))
	if err != nil {
		return s.dayStart(def)
	}
	return s.dayStart(t.Add(12 * time.Hour))
}

func (s *Server) dayStart(t time.Time) time.Time {
	if s.dayFrom != nil {
		return s.dayFrom(t)
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

type adminHomeData struct {
	Evaluation bool
	Blocks     []store.IdentityBlock
	Days       []evaluation.Day
	Day        time.Time
	Report     string
	Running    bool
	Last       evaluation.RunResult
	Audit      []store.AuditEntry
}

func (s *Server) adminHome(w http.ResponseWriter, r *http.Request, id Identity) {
	ctx := r.Context()
	d := adminHomeData{Day: s.day(r, "day", s.now())}
	var err error
	if s.identityGuard != nil {
		if d.Blocks, err = s.identityGuard.IdentityBlocks(ctx); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if s.admin != nil {
		d.Evaluation = true
		if d.Days, err = s.admin.Overview(ctx, 14); err != nil {
			s.serverError(w, r, err)
			return
		}
		if d.Report, err = s.admin.Report(ctx, d.Day); err != nil {
			s.serverError(w, r, err)
			return
		}
		if d.Audit, err = s.admin.Audit(ctx, 20); err != nil {
			s.serverError(w, r, err)
			return
		}
		d.Running, d.Last = s.admin.State()
	}
	notice := map[string]string{"started": "The evaluation is running. Reload the page in a few minutes.",
		"running": "The evaluation is already running."}[r.URL.Query().Get("notice")]
	s.render(w, r, http.StatusOK, "admin.html", view{User: id, Nav: "admin", Notice: notice, D: d})
}

func (s *Server) adminIdentityRelease(w http.ResponseWriter, r *http.Request, id Identity) {
	err := s.identityGuard.ReleaseIdentity(r.Context(), r.PathValue("uid"), r.FormValue("subject_hash"), id.UID)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusConflict, "Request refused", "The blocked identity has changed. Reload the admin page.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminReport(w http.ResponseWriter, r *http.Request, id Identity) {
	day := s.day(r, "day", s.now())
	md, err := s.admin.Report(r.Context(), day)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="evaluation-`+day.Format("2006-01-02")+`.md"`)
	w.Write([]byte(md))
}

func (s *Server) adminRun(w http.ResponseWriter, r *http.Request, id Identity) {
	hint := "started"
	if err := s.admin.Start(); errors.Is(err, evaluation.ErrRunning) {
		hint = "running"
	}
	http.Redirect(w, r, "/admin?notice="+hint, http.StatusSeeOther)
}

type adminChatsData struct {
	Truncated    bool
	From, To     time.Time
	Person, Flag string
	Flags        []string
	Persons      []string
	Chats        []evaluation.Item
	CostEUR      float64
	ToolErrors   int
}

func (s *Server) adminChats(w http.ResponseWriter, r *http.Request, id Identity) {
	q := r.URL.Query()
	d := adminChatsData{To: s.day(r, "to", s.now()), Flags: evaluation.AllFlags, Person: strings.TrimSpace(q.Get("person"))}
	d.From = s.day(r, "from", d.To.AddDate(0, 0, -13))
	if slices.Contains(evaluation.AllFlags, q.Get("flag")) {
		d.Flag = q.Get("flag")
	}
	all, err := s.admin.Chats(r.Context(), evaluation.Filter{From: d.From, To: s.dayStart(d.To.Add(36 * time.Hour))})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Truncated = all.Truncated
	for _, c := range all.Items {
		if !slices.Contains(d.Persons, c.Username) {
			d.Persons = append(d.Persons, c.Username)
		}
		if (d.Person == "" || c.Username == d.Person) && (d.Flag == "" || slices.Contains(c.Flags, d.Flag)) {
			d.Chats = append(d.Chats, c)
			d.CostEUR += c.CostEUR
			d.ToolErrors += c.ToolErrors
		}
	}
	slices.Sort(d.Persons)
	s.render(w, r, http.StatusOK, "admin-chats.html", view{User: id, Nav: "admin", D: d})
}

type transcriptLine struct{ Text, Kind string }

// transcriptLines classifies the stored transcript for display: speaker
// heads, tool lines (errors highlighted), cut marks, text.
func transcriptLines(t string) []transcriptLine {
	var out []transcriptLine
	for _, l := range strings.Split(strings.TrimSpace(t), "\n") {
		k := "text"
		switch {
		case strings.HasPrefix(l, "## "):
			k, l = "head", strings.TrimPrefix(l, "## ")
		case strings.HasPrefix(l, "- Werkzeug ") && strings.Contains(l, " (Fehler"), strings.HasPrefix(l, "Fehler: "):
			k = "error"
		case strings.HasPrefix(l, "- Werkzeug "):
			k = "tool"
		case strings.HasPrefix(l, "[... "):
			k = "truncated"
		}
		out = append(out, transcriptLine{l, k})
	}
	return out
}

func (s *Server) adminChat(w http.ResponseWriter, r *http.Request, id Identity) {
	c, err := s.admin.Chat(r.Context(), evaluation.Reader{UID: id.UID, Username: id.Username}, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Not found", "This chat does not exist in the evaluation (any more).")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin-chat.html", view{User: id, Nav: "admin", D: c})
}

var flagLabels = map[string]string{"tool-error": "Tool errors", "aborted": "Aborted", "model-error": "Model error",
	"unsuccessful": "not successful", "private-data": "private data?", "misuse": "misuse?", "unevaluated": "not evaluated"}
