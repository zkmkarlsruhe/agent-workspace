// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/evaluation"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Feedback stores messages to alice (store table feedback, shared with the
// chat tool feedback_send) and backs /admin/feedback; nil hides the form
// on the project page and in the help, and the admin list.
type Feedback interface {
	// Send stores a portal feedback; store.ErrLimit when the person sent too many.
	Send(ctx context.Context, uid, project, kind, text string) error
	List(ctx context.Context, n int) ([]store.Feedback, error)
	// Get records the read (audit) before returning the feedback with its chat excerpt.
	Get(ctx context.Context, r evaluation.Reader, id int64) (store.Feedback, error)
	SetDone(ctx context.Context, id int64, done bool) error
}

// FeedbackMaxText is the length limit of the form (same as mcp.FeedbackMaxText).
const FeedbackMaxText = 4000

var feedbackKinds = []string{"problem", "idea", "praise", "question"}

var feedbackLabels = map[string]string{"problem": "Problem", "idea": "Idea", "praise": "Praise", "question": "Question"}

func init() {
	pageFiles = append(pageFiles, "admin-feedback.html", "admin-feedback-detail.html")
}

func (s *Server) feedbackRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.feedback == nil {
		return
	}
	page("POST /feedback", s.feedbackSend)
}

// feedbackAdminRoutes: called by adminRoutes with its group check.
func (s *Server) feedbackAdminRoutes(a func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.feedback == nil {
		return
	}
	a("GET /admin/feedback", s.adminFeedback)
	a("GET /admin/feedback/{id}", s.adminFeedbackDetail)
	a("POST /admin/feedback/{id}/done", s.adminFeedbackDone)
}

// feedbackNotice: the 10 is mcp.FeedbackPerDay, the limit cmd/manager passes to the store.
func feedbackNotice(v string) string {
	return map[string]string{"sent": "Thank you. Your feedback has reached [[recipient]]; only he reads it.",
		"limit": "You have already sent 10 messages in the last 24 hours. More are possible later."}[v]
}

func (s *Server) feedbackSend(w http.ResponseWriter, r *http.Request, id Identity) {
	text, kind, project := strings.TrimSpace(r.FormValue("text")), r.FormValue("kind"), r.FormValue("project")
	if !slices.Contains(feedbackKinds, kind) {
		kind = "problem"
	}
	back := "/help"
	if project != "" {
		if ok, err := s.be.ProjectExists(r.Context(), id.UID, project); err != nil || !ok {
			project = ""
		} else {
			back = "/projects/" + url.PathEscape(project)
		}
	}
	if text == "" || utf8.RuneCountInString(text) > FeedbackMaxText {
		s.errorPage(w, r, http.StatusUnprocessableEntity, "Feedback not sent", "Please write what you want to tell [[recipient]] in the field, at most 4000 characters.")
		return
	}
	hint := "sent"
	if err := s.feedback.Send(r.Context(), id.UID, project, kind, text); errors.Is(err, store.ErrLimit) {
		hint = "limit"
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, back+"?feedback="+hint+"#feedback", http.StatusSeeOther)
}

type adminFeedbackData struct {
	Rows      []store.Feedback
	Truncated bool
}

func (s *Server) adminFeedback(w http.ResponseWriter, r *http.Request, id Identity) {
	l, err := s.feedback.List(r.Context(), 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin-feedback.html", view{User: id, Nav: "admin", D: adminFeedbackData{Rows: l, Truncated: len(l) == 200}})
}

func (s *Server) adminFeedbackDetail(w http.ResponseWriter, r *http.Request, id Identity) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.feedbackNotFound(w, r)
		return
	}
	f, err := s.feedback.Get(r.Context(), evaluation.Reader{UID: id.UID, Username: id.Username}, n)
	if errors.Is(err, store.ErrNotFound) {
		s.feedbackNotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin-feedback-detail.html", view{User: id, Nav: "admin", D: f})
}

func (s *Server) adminFeedbackDone(w http.ResponseWriter, r *http.Request, id Identity) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.feedbackNotFound(w, r)
		return
	}
	err = s.feedback.SetDone(r.Context(), n, r.FormValue("done") == "1")
	if errors.Is(err, store.ErrNotFound) {
		s.feedbackNotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/feedback", http.StatusSeeOther)
}

func (s *Server) feedbackNotFound(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusNotFound, "Not found", "This feedback does not exist (any more).")
}
