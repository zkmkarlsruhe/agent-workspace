// SPDX-License-Identifier: MIT

package portal

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"time"
)

// ownRE limits own skill names in URLs and notices to plain folder names.
var ownRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,63}$`)

// Tools is the core side of the Tools page (catalog opt-in, D13/D14).
// Separate from Backend so the frozen contract stays as it is; nil = no page.
type Tools interface {
	Tools(ctx context.Context, uid string) (ToolsPage, error)
	// SetTool switches a catalog item; ErrNotFound for unknown or standard ids.
	SetTool(ctx context.Context, uid, id string, on bool) (ToolEffect, error)
	// DeleteSkill removes an own skill folder for good; ErrNotRunning while asleep.
	DeleteSkill(ctx context.Context, uid, id string) (ToolEffect, error)
}

type ToolsPage struct {
	ProjectScoped bool
	Running       bool       // standard and own skills are only known while the environment runs
	Standard      []Tool     // always on: base skills and standard MCP items
	Catalog       []Tool     // switchable, On set
	Own           []OwnSkill // real folders in ~/.config/opencode/skills
}

type Tool struct {
	ID, Title, Description, Category string
	// English display texts from the catalog; "" = the German one is shown
	TitleEN, DescriptionEN string
	On                     bool
}

// localize puts the English display texts in place for lang=en; the
// category is one of a fixed list (catalog lint) and comes from the catalog.
func localize(l string, ts []Tool) []Tool {
	if l != langEN {
		return ts
	}
	ts = slices.Clone(ts)
	for i := range ts {
		ts[i].Title = cmp.Or(ts[i].TitleEN, ts[i].Title)
		ts[i].Description = cmp.Or(ts[i].DescriptionEN, ts[i].Description)
		ts[i].Category = cmp.Or(categoryEN[ts[i].Category], ts[i].Category)
	}
	return ts
}

type OwnSkill struct {
	ID, Description string
	Changed         time.Time
}

// ToolEffect says when a change reaches the person's helper.
type ToolEffect int

const (
	EffectNow       ToolEffect = iota // reloaded: every new chat has it
	EffectBusy                        // saved change pending an idle reload or retry
	EffectNextStart                   // environment asleep: when it starts next
)

func init() {
	pageFiles = append(pageFiles, "tools.html", "tools-delete.html")
}

func (s *Server) toolsRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.tools == nil {
		return
	}
	page("GET /tools", s.toolsPage)
	page("POST /tools/{id}", s.toolToggle)
	page("GET /tools/own/{id}/delete", s.ownDeleteConfirm)
	page("POST /tools/own/{id}/delete", s.ownDelete)
}

var effectQuery = map[ToolEffect]string{EffectNow: "now", EffectBusy: "restart", EffectNextStart: "next-start"}

func effectText(q string) string {
	switch q {
	case "now":
		return "This applies from your next conversation."
	case "restart":
		return "The change is saved and will be applied automatically when the helper is ready."
	case "next-start":
		return "Your workspace is stopped. The change applies when you start it."
	}
	return ""
}

func (s *Server) toolsPage(w http.ResponseWriter, r *http.Request, id Identity) {
	d, err := s.tools.Tools(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Standard, d.Catalog = localize(lang(r), d.Standard), localize(lang(r), d.Catalog)
	v := view{User: id, Nav: "tools", D: d}
	q := r.URL.Query()
	if sid := q.Get("toggled"); sid != "" {
		for _, t := range d.Catalog {
			if t.ID == sid {
				msg := "“%s” is switched off. %s"
				if t.On {
					msg = "“%s” is switched on. %s"
				}
				v.Notice = trf(lang(r), msg, t.Title, tr(lang(r), effectText(q.Get("effect"))))
			}
		}
	}
	if gid := q.Get("deleted"); gid != "" && ownRE.MatchString(gid) {
		v.Notice = trf(lang(r), "The instructions “%s” have been deleted. %s", gid, tr(lang(r), effectText(q.Get("effect"))))
	}
	s.render(w, r, http.StatusOK, "tools.html", v)
}

func (s *Server) toolToggle(w http.ResponseWriter, r *http.Request, id Identity) {
	tid := r.PathValue("id")
	eff, err := s.tools.SetTool(r.Context(), id.UID, tid, r.FormValue("on") == "1")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/tools?toggled="+url.QueryEscape(tid)+"&effect="+effectQuery[eff], http.StatusSeeOther)
}

// ownSkill finds an own skill by its folder name; a 404 page otherwise.
func (s *Server) ownSkill(w http.ResponseWriter, r *http.Request, id Identity) (OwnSkill, bool) {
	d, err := s.tools.Tools(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return OwnSkill{}, false
	}
	if !d.Running {
		s.serverError(w, r, ErrNotRunning)
		return OwnSkill{}, false
	}
	for _, o := range d.Own {
		if o.ID == r.PathValue("id") {
			return o, true
		}
	}
	s.errorPage(w, r, http.StatusNotFound, "Not found", "These saved instructions no longer exist.")
	return OwnSkill{}, false
}

func (s *Server) ownDeleteConfirm(w http.ResponseWriter, r *http.Request, id Identity) {
	if o, ok := s.ownSkill(w, r, id); ok {
		s.render(w, r, http.StatusOK, "tools-delete.html", view{User: id, Nav: "tools", D: o})
	}
}

func (s *Server) ownDelete(w http.ResponseWriter, r *http.Request, id Identity) {
	o, ok := s.ownSkill(w, r, id)
	if !ok {
		return
	}
	if r.FormValue("confirmed") != "yes" {
		s.render(w, r, http.StatusUnprocessableEntity, "tools-delete.html", view{User: id, Nav: "tools", D: o,
			Errors: []fieldError{{"confirmed", "Please confirm that the skill should be deleted."}}})
		return
	}
	eff, err := s.tools.DeleteSkill(r.Context(), id.UID, o.ID)
	if errors.Is(err, ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Not found", "These saved instructions no longer exist.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/tools?deleted="+url.QueryEscape(o.ID)+"&effect="+effectQuery[eff], http.StatusSeeOther)
}

// categoryEN: the catalog delivers the German label of its fixed category list
// (deploy/etc-opencode/tests/lint.py).
var categoryEN = map[string]string{
	"Texte":            "Texts",
	"Tabellen":         "Spreadsheets",
	"Präsentation":     "Presentations",
	"Daten":            "Data",
	"Bilder":           "Images",
	"Recherche":        "Research",
	"Organisation":     "Organisation",
	"Workspace-Wissen": "Workspace knowledge",
}
