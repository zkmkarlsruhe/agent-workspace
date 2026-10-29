// SPDX-License-Identifier: MIT

package portal

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func init() { pageFiles = append(pageFiles, "assistant-gaps.html", "admin-assistant-gaps.html") }
func (s *Server) gapRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.taskAssistants == nil || s.taskAssistants.Store == nil {
		return
	}
	page("GET /projects/{name}/gaps", s.assistantGaps)
	page("POST /projects/{name}/gaps", s.assistantGaps)
	page("POST /projects/{name}/gaps/{gap}/withdraw", s.withdrawGap)
	page("GET /assistant-gaps/export", s.exportGaps)
	page("GET /assistant-gaps", s.ownGaps)
	page("POST /assistant-gaps/{gap}/withdraw", s.withdrawGap)
}
func (s *Server) gapAdminRoutes(a func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	a("GET /admin/assistant-gaps", s.adminGaps)
	a("POST /admin/assistant-gaps/{gap}", s.answerGap)
}

type assistantGapsData struct {
	Project            store.Project
	Rows               []store.AssistantGap
	Gap                store.AssistantGap
	Preview, CanSubmit bool
	Error              string
}

func (s *Server) assistantGaps(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	p, err := s.taskAssistants.Store.ProjectByFolder(r.Context(), id.UID, name)
	if err != nil || p.Assistant.ID == "" {
		s.editorError(w, r, store.ErrNotFound)
		return
	}
	def, exists := instance.Current().Assistant(p.Assistant.ID)
	d := assistantGapsData{Project: p, CanSubmit: exists && def.GapCollection && def.Available(id.Groups) && instance.Current().AssistantPin(def) == p.Assistant}
	status := 200
	if r.Method == http.MethodPost {
		if !d.CanSubmit {
			s.editorError(w, r, store.ErrNotFound)
			return
		}
		if err := editorForm(w, r); err != nil {
			s.editorError(w, r, err)
			return
		}
		d.Gap = store.AssistantGap{ID: r.PostForm.Get("token"), UID: id.UID, ProjectID: p.ID, Summary: r.PostForm.Get("summary"), Sources: r.PostForm.Get("sources"), Category: r.PostForm.Get("category")}
		if d.Gap.ID == "" {
			d.Gap.ID = uuid.NewString()
		}
		err = d.Gap.Validate()
		if err == nil && r.PostForm.Get("approve") == "true" {
			err = s.taskAssistants.Store.AddAssistantGap(r.Context(), d.Gap)
			if err == nil {
				http.Redirect(w, r, "/projects/"+name+"/gaps", 303)
				return
			}
		}
		if err != nil {
			status = 422
			d.Error = err.Error()
		} else {
			d.Preview = r.PostForm.Get("edit") != "true"
		}
	}
	d.Rows, err = s.taskAssistants.Store.AssistantGaps(r.Context(), id.UID, p.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "assistant-gaps.html", view{User: id, Nav: "projects", D: d})
}
func (s *Server) withdrawGap(w http.ResponseWriter, r *http.Request, id Identity) {
	if err := s.taskAssistants.Store.DeleteAssistantGap(r.Context(), id.UID, r.PathValue("gap")); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/assistant-gaps", 303)
}
func (s *Server) exportGaps(w http.ResponseWriter, r *http.Request, id Identity) {
	w.Header().Set("Cache-Control", "no-store")
	rows, err := s.taskAssistants.Store.AssistantGaps(r.Context(), id.UID, "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="knowledge-gaps.json"`)
	_ = json.NewEncoder(w).Encode(rows)
}
func (s *Server) adminGaps(w http.ResponseWriter, r *http.Request, id Identity) {
	// The read itself is audited before any personal summary is returned.
	if err := s.taskAssistants.Store.AuditAssistant(r.Context(), id.UID, "gaps", 0, "", "assistant_gaps_read"); err != nil {
		s.serverError(w, r, err)
		return
	}
	rows, err := s.taskAssistants.Store.AssistantGaps(r.Context(), "", "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, 200, "admin-assistant-gaps.html", view{User: id, Nav: "assistant-admin", D: rows})
}
func (s *Server) answerGap(w http.ResponseWriter, r *http.Request, id Identity) {
	if err := editorForm(w, r); err != nil {
		s.editorError(w, r, err)
		return
	}
	revision, err := strconv.Atoi(r.PostForm.Get("revision"))
	if err == nil {
		err = s.taskAssistants.Store.AnswerAssistantGap(r.Context(), id.UID, r.PathValue("gap"), revision, r.PostForm.Get("guidance"))
	}
	if err != nil {
		s.editorError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/assistant-gaps", 303)
}

func (s *Server) ownGaps(w http.ResponseWriter, r *http.Request, id Identity) {
	rows, err := s.taskAssistants.Store.AssistantGaps(r.Context(), id.UID, "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, 200, "assistant-gaps.html", view{User: id, Nav: "projects", D: assistantGapsData{Rows: rows}})
}
