// SPDX-License-Identifier: MIT

package portal

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/convert"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func init() {
	pageFiles = append(pageFiles, "teams.html", "team.html", "knowledge.html", "knowledge-collection.html", "knowledge-page.html")
}

type teamsData struct {
	Actor      store.KnowledgeActor
	Teams      []store.Team
	Team       store.Team
	Members    []store.TeamMember
	Candidates []store.TeamCandidate
	Audit      []store.TeamAudit
	Query      string
	Recovery   bool
}
type knowledgeData struct {
	Actor                 store.KnowledgeActor
	Collections           []store.KnowledgeCollection
	Collection            store.KnowledgeCollection
	Teams                 []store.Team
	Pages, History        []store.KnowledgePage
	Page                  store.KnowledgePage
	Current               int
	Preview               template.HTML
	Hits                  []store.KnowledgeHit
	Query, Error, Project string
	Bound                 []store.KnowledgeCollection
	Projects              []store.Project
}

func (s *Server) knowledgeRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.knowledge == nil {
		return
	}
	write := func(pattern string, h func(http.ResponseWriter, *http.Request, Identity)) {
		page(pattern, func(w http.ResponseWriter, r *http.Request, id Identity) {
			if s.knowledgeReadOnly {
				s.knowledgeError(w, r, store.ErrKnowledgeReadOnly)
				return
			}
			h(w, r, id)
		})
	}
	page("GET /teams", s.teamsList)
	write("POST /teams", s.teamCreate)
	page("GET /teams/{team}", s.teamDetail)
	page("POST /teams/{team}", s.teamUpdate)
	page("POST /teams/{team}/members", s.teamMember)
	page("POST /teams/{team}/invitation", s.teamInvitation)
	page("POST /teams/{team}/purge", s.teamPurge)
	page("GET /knowledge", s.knowledgeList)
	write("POST /knowledge", s.knowledgeCreate)
	write("POST /knowledge/import", s.knowledgeImport)
	page("GET /knowledge/{collection}", s.knowledgeCollection)
	page("POST /knowledge/{collection}", s.knowledgeSettings)
	page("POST /knowledge/{collection}/purge", s.knowledgePurge)
	page("GET /knowledge/{collection}/export", s.knowledgeExport)
	page("GET /knowledge/{collection}/pages/new", s.knowledgeNewPage)
	page("GET /knowledge/{collection}/pages/{page}", s.knowledgePage)
	page("GET /knowledge/{collection}/pages/{page}/download", s.knowledgeDownload)
	write("POST /knowledge/{collection}/markdown", s.knowledgeMarkdown)
	write("POST /knowledge/{collection}/pages/{page}", s.knowledgeSave)
	write("POST /knowledge/{collection}/pages/{page}/restore", s.knowledgeRestore)
	page("POST /knowledge/{collection}/pages/{page}/purge", s.knowledgePagePurge)
	write("POST /knowledge/bind", s.knowledgeBind)
	// Recovery has its own role predicate; it does not enable evaluation routes.
	admin := func(h func(http.ResponseWriter, *http.Request, Identity)) func(http.ResponseWriter, *http.Request, Identity) {
		return func(w http.ResponseWriter, r *http.Request, id Identity) {
			if s.adminGroup == "" || !slices.Contains(id.Groups, s.adminGroup) {
				s.knowledgeError(w, r, store.ErrNotFound)
				return
			}
			h(w, r, id)
		}
	}
	page("GET /teams-recovery", admin(s.teamsRecovery))
	page("POST /teams-recovery/{team}", admin(s.teamRecover))
}
func (s *Server) knowledgeActor(w http.ResponseWriter, r *http.Request, id Identity, post bool) (store.KnowledgeActor, bool) {
	w.Header().Set("Cache-Control", "no-store")
	a, e := s.knowledge.KnowledgeActor(r.Context(), id.UID)
	if e != nil {
		s.knowledgeError(w, r, e)
		return a, false
	}
	if post {
		r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
		if e = r.ParseForm(); e != nil {
			s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
			return a, false
		}
		if r.PostForm.Get("account") != a.Stamp {
			s.knowledgeError(w, r, store.ErrNotFound)
			return a, false
		}
	}
	return a, true
}
func (s *Server) knowledgeError(w http.ResponseWriter, r *http.Request, e error) {
	switch {
	case errors.Is(e, store.ErrNotFound):
		s.errorPage(w, r, 404, "Page not found", "This page does not exist.")
	case errors.Is(e, store.ErrKnowledgeConflict):
		s.errorPage(w, r, 409, "Changes conflict", e.Error())
	case errors.Is(e, store.ErrKnowledgeReadOnly), errors.Is(e, store.ErrLastTeamAdmin):
		s.errorPage(w, r, 409, "Change refused", e.Error())
	case errors.Is(e, store.ErrTeamMembershipLimit), errors.Is(e, store.ErrTeamInviteLimit), errors.Is(e, store.ErrTeamSearchLimit):
		s.errorPage(w, r, http.StatusTooManyRequests, "Change refused", e.Error())
	case errors.Is(e, store.ErrKnowledgeInvalid):
		s.errorPage(w, r, 400, "Check your entries", e.Error())
	default:
		s.serverError(w, r, e)
	}
}
func knowledgeRevision(r *http.Request) int {
	n, e := strconv.Atoi(r.PostForm.Get("revision"))
	if e != nil || n < 0 {
		return -1
	}
	return n
}
func knowledgeURL(id string) string       { return "/knowledge/" + url.PathEscape(id) }
func knowledgePageURL(c, p string) string { return knowledgeURL(c) + "/pages/" + url.PathEscape(p) }

func (s *Server) teamsList(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, false)
	if !ok {
		return
	}
	rows, e := s.knowledge.Teams(r.Context(), a)
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	s.render(w, r, 200, "teams.html", view{User: id, Nav: "teams", D: teamsData{Actor: a, Teams: rows}})
}
func (s *Server) teamCreate(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	t, e := s.knowledge.CreateTeam(r.Context(), a, r.PostForm.Get("name"), r.PostForm.Get("description"))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, "/teams/"+t.ID, 303)
}
func (s *Server) teamDetail(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, false)
	if !ok {
		return
	}
	d := teamsData{Actor: a, Query: r.URL.Query().Get("q")}
	var e error
	d.Team, d.Members, d.Audit, e = s.knowledge.Team(r.Context(), a, r.PathValue("team"))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	if d.Team.Role == "admin" {
		d.Candidates, e = s.knowledge.TeamCandidates(r.Context(), a, d.Team.ID, d.Query)
		if e != nil {
			s.knowledgeError(w, r, e)
			return
		}
	}
	for i := range d.Audit {
		label := map[string]string{"invite": "Invitation sent", "accept": "Invitation accepted", "decline": "Invitation declined", "withdraw": "Invitation withdrawn", "create": "Team created", "role:member": "Member assigned", "role:admin": "Administrator appointed", "remove": "Membership removed", "settings": "Team settings updated", "recover": "Administration recovered"}[d.Audit[i].Action]
		if strings.HasPrefix(d.Audit[i].Action, "purge-") {
			label = "Knowledge permanently deleted"
		}
		d.Audit[i].Action = tr(lang(r), label)
	}
	s.render(w, r, 200, "team.html", view{User: id, Nav: "teams", D: d})
}
func (s *Server) teamUpdate(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	e := s.knowledge.UpdateTeam(r.Context(), a, r.PathValue("team"), r.PostForm.Get("name"), r.PostForm.Get("description"), r.PostForm.Get("archived") == "true", knowledgeRevision(r))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, "/teams/"+url.PathEscape(r.PathValue("team")), 303)
}
func (s *Server) teamMember(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	e := s.knowledge.SetTeamMember(r.Context(), a, r.PathValue("team"), r.PostForm.Get("member"), r.PostForm.Get("member_stamp"), r.PostForm.Get("role"), knowledgeRevision(r))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, "/teams", 303)
}
func (s *Server) teamsRecovery(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, false)
	if !ok {
		return
	}
	rows, e := s.knowledge.OrphanTeams(r.Context(), a)
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	s.render(w, r, 200, "teams.html", view{User: id, Nav: "teams", D: teamsData{Actor: a, Teams: rows, Recovery: true}})
}
func (s *Server) teamRecover(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	e := s.knowledge.RecoverTeam(r.Context(), a, r.PathValue("team"), r.PostForm.Get("username"), knowledgeRevision(r))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, "/teams-recovery", 303)
}

func (s *Server) knowledgeList(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, false)
	if !ok {
		return
	}
	d := knowledgeData{Actor: a, Query: r.URL.Query().Get("q"), Project: r.URL.Query().Get("project")}
	if name := r.URL.Query().Get("project_name"); d.Project == "" && name != "" {
		p, e := s.knowledge.ProjectByFolder(r.Context(), a.UID, name)
		if e != nil {
			s.knowledgeError(w, r, e)
			return
		}
		d.Project = p.ID
	}

	var e error
	d.Collections, e = s.knowledge.KnowledgeCollections(r.Context(), a)
	if e == nil {
		d.Teams, e = s.knowledge.Teams(r.Context(), a)
	}
	if e == nil && d.Query != "" {
		d.Hits, e = s.knowledge.SearchKnowledge(r.Context(), a, "", d.Query)
	}
	if e == nil && d.Project != "" {
		d.Bound, e = s.knowledge.ProjectKnowledge(r.Context(), a, d.Project)
	}
	if e == nil {
		d.Projects, e = s.knowledge.Projects(r.Context(), a.UID)
	}
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	s.render(w, r, 200, "knowledge.html", view{User: id, Nav: "knowledge", D: d})
}
func (s *Server) knowledgeCreate(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	c, e := s.knowledge.CreateKnowledgeCollection(r.Context(), a, r.PostForm.Get("team"), r.PostForm.Get("title"), r.PostForm.Get("description"))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, knowledgeURL(c.ID), 303)
}
func (s *Server) knowledgeCollection(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, false)
	if !ok {
		return
	}
	d := knowledgeData{Actor: a, Query: r.URL.Query().Get("q"), Page: store.KnowledgePage{ID: uuid.NewString()}}
	var e error
	d.Collection, e = s.knowledge.KnowledgeCollection(r.Context(), a, r.PathValue("collection"))
	d.Collection.Writable = d.Collection.Writable && !s.knowledgeReadOnly
	if e == nil {
		d.Pages, e = s.knowledge.KnowledgePages(r.Context(), a, d.Collection.ID)
	}
	if e == nil && d.Query != "" {
		d.Hits, e = s.knowledge.SearchKnowledge(r.Context(), a, d.Collection.ID, d.Query)
	}
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	s.render(w, r, 200, "knowledge-collection.html", view{User: id, Nav: "knowledge", D: d})
}
func (s *Server) knowledgeSettings(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	e := s.knowledge.UpdateKnowledgeCollection(r.Context(), a, r.PathValue("collection"), r.PostForm.Get("title"), r.PostForm.Get("description"), r.PostForm.Get("archived") == "true", knowledgeRevision(r))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, knowledgeURL(r.PathValue("collection")), 303)
}
func (s *Server) knowledgePurge(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	if r.PostForm.Get("confirm") != "purge" {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	e := s.knowledge.PurgeKnowledgeCollection(r.Context(), a, r.PathValue("collection"), knowledgeRevision(r))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, "/knowledge", 303)
}
func (s *Server) knowledgeExport(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, false)
	if !ok {
		return
	}
	b, e := s.knowledge.ExportKnowledge(r.Context(), a, r.PathValue("collection"))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	data, e := b.ZIP()
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	// Encoding may take time; verify that membership still permits export before delivery.
	c, e := s.knowledge.KnowledgeCollection(r.Context(), a, r.PathValue("collection"))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	if c.Role != "admin" {
		s.knowledgeError(w, r, store.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="knowledge.zip"`)
	_, _ = w.Write(data)
}
func (s *Server) knowledgeImport(w http.ResponseWriter, r *http.Request, id Identity) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, store.KnowledgeMaxBytes+(4<<20))
	if e := r.ParseMultipartForm(1 << 20); e != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(e, &tooLarge) {
			s.errorPage(w, r, http.StatusRequestEntityTooLarge, "Import too large", "Knowledge import body exceeds 20 MiB.")
			return
		}
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	a, e := s.knowledge.KnowledgeActor(r.Context(), id.UID)
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	if r.PostForm.Get("account") != a.Stamp {
		s.knowledgeError(w, r, store.ErrNotFound)
		return
	}
	f, _, e := r.FormFile("manifest")
	if e != nil {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	defer f.Close()
	var b store.KnowledgeBundle
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if e = dec.Decode(&b); e != nil {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	c, e := s.knowledge.ImportKnowledge(r.Context(), a, r.PostForm.Get("team"), b)
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, knowledgeURL(c.ID), 303)
}
func (s *Server) knowledgeNewPage(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, false)
	if !ok {
		return
	}
	s.renderKnowledgePage(w, r, id, a, store.KnowledgePage{ID: uuid.NewString(), CollectionID: r.PathValue("collection")}, 200, "")
}
func (s *Server) knowledgePage(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, false)
	if !ok {
		return
	}
	rev := 0
	if raw := r.URL.Query().Get("revision"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 {
			s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
			return
		}
		rev = n
	}
	p, e := s.knowledge.KnowledgePage(r.Context(), a, r.PathValue("collection"), r.PathValue("page"), rev)
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	s.renderKnowledgePage(w, r, id, a, p, 200, "")
}
func (s *Server) renderKnowledgePage(w http.ResponseWriter, r *http.Request, id Identity, a store.KnowledgeActor, p store.KnowledgePage, status int, message string) {
	d := knowledgeData{Actor: a, Page: p, Error: tr(lang(r), message)}
	var e error
	d.Collection, e = s.knowledge.KnowledgeCollection(r.Context(), a, p.CollectionID)
	d.Collection.Writable = d.Collection.Writable && !s.knowledgeReadOnly
	if e == nil {
		d.Pages, e = s.knowledge.KnowledgePages(r.Context(), a, p.CollectionID)
	}
	if e == nil && p.Revision > 0 {
		var current store.KnowledgePage
		current, e = s.knowledge.KnowledgePage(r.Context(), a, p.CollectionID, p.ID, 0)
		d.Current = current.Revision
		if e == nil {
			d.History, e = s.knowledge.KnowledgeHistory(r.Context(), a, p.CollectionID, p.ID)
		}
	}
	if e == nil {
		d.Preview, e = convert.Fragment([]byte(p.Body))
	}
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	s.render(w, r, status, "knowledge-page.html", view{User: id, Nav: "knowledge", D: d})
}
func (s *Server) knowledgeSave(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	p := store.KnowledgePage{ID: r.PathValue("page"), CollectionID: r.PathValue("collection"), Revision: knowledgeRevision(r), Title: r.PostForm.Get("title"), Body: r.PostForm.Get("body"), Parent: r.PostForm.Get("parent"), Sources: r.PostForm.Get("sources"), Reason: r.PostForm.Get("reason"), Archived: r.PostForm.Get("archived") == "true"}
	if r.PostForm.Get("preview") == "true" {
		if len(p.Body) > store.KnowledgeMaxBody {
			s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
			return
		}
		s.renderKnowledgePage(w, r, id, a, p, 200, "")
		return
	}
	expected := p.Revision
	if r.PostForm.Get("reviewed") == "true" {
		n, e := strconv.Atoi(r.PostForm.Get("current_revision"))
		if e != nil || n < 1 {
			s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
			return
		}
		expected = n
	}
	saved, e := s.knowledge.SaveKnowledgePage(r.Context(), a, p, expected)
	if errors.Is(e, store.ErrKnowledgeConflict) || errors.Is(e, store.ErrKnowledgeInvalid) {
		status := 400
		if errors.Is(e, store.ErrKnowledgeConflict) {
			status = 409
		}
		s.renderKnowledgePage(w, r, id, a, p, status, e.Error())
		return
	}
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, knowledgePageURL(saved.CollectionID, saved.ID), 303)
}
func (s *Server) knowledgeRestore(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	rev, e := strconv.Atoi(r.PostForm.Get("restore"))
	if e != nil || rev < 1 {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	p, e := s.knowledge.RestoreKnowledgeRevision(r.Context(), a, r.PathValue("collection"), r.PathValue("page"), rev, knowledgeRevision(r))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, knowledgePageURL(p.CollectionID, p.ID), 303)
}
func (s *Server) knowledgePagePurge(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	if r.PostForm.Get("confirm") != "purge" {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	e := s.knowledge.PurgeKnowledgePage(r.Context(), a, r.PathValue("collection"), r.PathValue("page"), knowledgeRevision(r))
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, knowledgeURL(r.PathValue("collection")), 303)
}
func (s *Server) knowledgeBind(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	project := r.PostForm.Get("project")
	e := s.knowledge.BindKnowledge(r.Context(), a, project, r.PostForm.Get("collection"), r.PostForm.Get("on") == "true")
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, "/knowledge?project="+url.QueryEscape(project), 303)
}

func (s *Server) knowledgeDownload(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, false)
	if !ok {
		return
	}
	revision, e := strconv.Atoi(r.URL.Query().Get("revision"))
	if e != nil || revision < 1 {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	p, e := s.knowledge.KnowledgePage(r.Context(), a, r.PathValue("collection"), r.PathValue("page"), revision)
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="knowledge-page.md"`)
	_, _ = io.WriteString(w, "# "+p.Title+"\n\n"+p.Body+"\n\n## "+tr(lang(r), "Sources")+"\n\n"+p.Sources+"\n")
}
func (s *Server) knowledgeMarkdown(w http.ResponseWriter, r *http.Request, id Identity) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if e := r.ParseMultipartForm(256 << 10); e != nil {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	a, e := s.knowledge.KnowledgeActor(r.Context(), id.UID)
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	if r.PostForm.Get("account") != a.Stamp {
		s.knowledgeError(w, r, store.ErrNotFound)
		return
	}
	file, _, e := r.FormFile("markdown")
	if e != nil {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	defer file.Close()
	body, e := io.ReadAll(io.LimitReader(file, store.KnowledgeMaxBody+1))
	if e != nil || len(body) > store.KnowledgeMaxBody {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	p, e := s.knowledge.SaveKnowledgePage(r.Context(), a, store.KnowledgePage{ID: r.PostForm.Get("page"), CollectionID: r.PathValue("collection"), Title: r.PostForm.Get("title"), Body: string(body), Sources: r.PostForm.Get("sources"), Reason: tr(lang(r), "Markdown import")}, 0)
	if e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, knowledgePageURL(p.CollectionID, p.ID), 303)
}

func (s *Server) teamPurge(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	if r.PostForm.Get("confirm") != "purge" {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	if e := s.knowledge.PurgeTeam(r.Context(), a, r.PathValue("team"), knowledgeRevision(r)); e != nil {
		s.knowledgeError(w, r, e)
		return
	}
	http.Redirect(w, r, "/teams", 303)
}
