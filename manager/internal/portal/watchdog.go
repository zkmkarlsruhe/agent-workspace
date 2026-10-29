// SPDX-License-Identifier: MIT

package portal

import (
	"bytes"
	"net/http"
	"time"
)

type WatchdogNotice struct {
	SessionID     string
	ParentID      string
	Title         string
	Directory     string
	SilentSeconds int
	Attempt       int
	Retry         bool
	Next          time.Time
}

type watchdogRow struct {
	WatchdogNotice
	Minutes int
	Seconds int
}

type watchdogView struct {
	URL  string
	Rows []watchdogRow
}

func (s *Server) watchdogView(r *http.Request, uid, project string) (watchdogView, error) {
	v := watchdogView{URL: "/watchdog"}
	if project != "" {
		v.URL = "/projects/" + project + "/watchdog"
	}
	notices, err := s.be.Watchdog(r.Context(), uid, project)
	for _, n := range notices {
		v.Rows = append(v.Rows, watchdogRow{WatchdogNotice: n, Minutes: n.SilentSeconds / 60,
			Seconds: max(0, int(n.Next.Sub(s.now()).Seconds()))})
	}
	return v, err
}

func (s *Server) watchdogFragment(w http.ResponseWriter, r *http.Request, id Identity) {
	project := r.PathValue("name")
	if project != "" {
		if _, ok := s.projectName(w, r); !ok {
			return
		}
	}
	v, err := s.watchdogView(r, id.UID, project)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := s.pages[lang(r)]["project.html"].ExecuteTemplate(&buf, "watchdog", v); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) watchdogStop(w http.ResponseWriter, r *http.Request, id Identity) {
	project := r.PathValue("name")
	if project != "" {
		if _, ok := s.projectName(w, r); !ok {
			return
		}
	}
	if err := s.be.StopSilentSession(r.Context(), id.UID, project, r.PathValue("session")); err != nil {
		s.serverError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		s.watchdogFragment(w, r, id)
		return
	}
	to := "/environment"
	if project != "" {
		to = "/projects/" + project
	}
	navigate(w, r, to)
}
