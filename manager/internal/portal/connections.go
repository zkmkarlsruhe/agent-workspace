// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Connections is the core side of the Connections page (house services,
// docs/adr/README.md §3.3.5). Separate from Backend like Tools; nil = no page.
type Connections interface {
	List(ctx context.Context, uid string) ([]Connection, error)
	// Connect starts the login at the service and returns the URL the
	// person opens there. ErrNotFound for an unknown id, ErrUnavailable when
	// the service does not answer.
	Connect(ctx context.Context, uid, username, id string) (loginURL string, err error)
	// Disconnect revokes the login at the service, then forgets it; ErrUnavailable
	// when the service does not answer (the login then stays stored).
	Disconnect(ctx context.Context, uid, id string) error
}

// Onboarding remembers that the Onboarding was shown; nil = no Onboarding.
type Onboarding interface {
	Seen(ctx context.Context, uid string) (bool, error)
	MarkSeen(ctx context.Context, uid string) error
}

type Connection struct {
	ID, Title string
	Connected bool
	Expired   bool // the service refused the stored login (401)
	Login     string
	Since     time.Time
	Flow      Flow
}

// Flow is the last connect attempt; Ended tells the portal whether the
// result is still worth showing.
type Flow struct {
	State    FlowState
	LoginURL string
	Ended    time.Time
}

type FlowState string

const (
	FlowNone     FlowState = ""
	FlowPending  FlowState = "pending"
	FlowDone     FlowState = "done"
	FlowMismatch FlowState = "mismatch"
	FlowFailed   FlowState = "failed"
	FlowTimeout  FlowState = "timeout"
)

// Onboarding has three steps; step 2 lists the house services.
const onboardingSteps = 3

func init() {
	pageFiles = append(pageFiles, "connections.html", "connection.html", "onboarding.html")
}

func (s *Server) connectionsRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.onboard != nil {
		page("GET /onboarding", s.onboarding)
	}
	if s.conns == nil {
		return
	}
	page("GET /connections", s.connections)
	page("GET /connections/{id}", s.connection)
	page("POST /connections/{id}/connect", s.connect)
	page("POST /connections/{id}/disconnect", s.disconnect)
}

// shownFlow hides an old failed attempt: it explains the last click, not
// something from yesterday.
func (s *Server) shownFlow(v *Connection) {
	if v.Flow.State != FlowPending && v.Flow.State != FlowNone && s.now().Sub(v.Flow.Ended) > time.Hour {
		v.Flow = Flow{}
	}
}

func (s *Server) connections(w http.ResponseWriter, r *http.Request, id Identity) {
	vs, err := s.conns.List(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for i := range vs {
		s.shownFlow(&vs[i])
	}
	v := view{User: id, Nav: "connections", D: vs}
	if gid := r.URL.Query().Get("disconnected"); gid != "" {
		for _, c := range vs {
			if c.ID == gid {
				v.Notice = trf(lang(r), "%s is disconnected. The access has been deleted there.", c.Title)
			}
		}
	}
	s.render(w, r, http.StatusOK, "connections.html", v)
}

type connectionData struct {
	Connection
	Onboarding bool // reached from step 2 of the Onboarding
}

func (s *Server) find(w http.ResponseWriter, r *http.Request, id Identity) (Connection, bool) {
	vs, err := s.conns.List(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return Connection{}, false
	}
	for _, v := range vs {
		if v.ID == r.PathValue("id") {
			return v, true
		}
	}
	s.errorPage(w, r, http.StatusNotFound, "Not found", "This connection does not exist here.")
	return Connection{}, false
}

// verbindung is the page between "Verbinden" and the grant in the other tab.
// While the attempt is pending it polls itself (htmx, every 2 s).
func (s *Server) connection(w http.ResponseWriter, r *http.Request, id Identity) {
	v, ok := s.find(w, r, id)
	if !ok {
		return
	}
	s.shownFlow(&v)
	if v.Flow.State == FlowNone {
		http.Redirect(w, r, "/connections", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "connection.html", view{User: id, Nav: "connections",
		D: connectionData{Connection: v, Onboarding: r.URL.Query().Get("from") == "onboarding"}})
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request, id Identity) {
	v, ok := s.find(w, r, id)
	if !ok {
		return
	}
	if _, err := s.conns.Connect(r.Context(), id.UID, id.Username, v.ID); err != nil {
		s.unreachable(w, r, v, err)
		return
	}
	target := "/connections/" + url.PathEscape(v.ID)
	if r.FormValue("from") == "onboarding" {
		target += "?from=onboarding"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request, id Identity) {
	v, ok := s.find(w, r, id)
	if !ok {
		return
	}
	if err := s.conns.Disconnect(r.Context(), id.UID, v.ID); err != nil {
		s.unreachable(w, r, v, err)
		return
	}
	http.Redirect(w, r, "/connections?disconnected="+url.QueryEscape(v.ID), http.StatusSeeOther)
}

func (s *Server) unreachable(w http.ResponseWriter, r *http.Request, v Connection, err error) {
	if errors.Is(err, ErrUnavailable) {
		s.errorPage(w, r, http.StatusBadGateway, trf(lang(r), "%s cannot be reached right now", v.Title),
			trf(lang(r), "%s did not respond. Nothing has changed in your connection. Try again in a few minutes.", v.Title))
		return
	}
	s.serverError(w, r, err)
}

type onboardingData struct {
	Step, Steps int
	Settings    bool // step 3 is the Settings, the first task moves to step 4
	Connections []Connection
	Templates   []taskTemplate
}

// onboarding is the short guided setup: shown once on the first visit
// (home redirects there), re-openable from Environment and Help.
func (s *Server) onboarding(w http.ResponseWriter, r *http.Request, id Identity) {
	n, _ := strconv.Atoi(r.URL.Query().Get("step"))
	d := onboardingData{Step: min(max(n, 1), s.onboardingStepCount()), Steps: s.onboardingStepCount(), Settings: s.settings != nil, Templates: taskTemplates}
	if d.Step == 2 && s.conns != nil {
		vs, err := s.conns.List(r.Context(), id.UID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for i := range vs {
			s.shownFlow(&vs[i])
		}
		d.Connections = vs
	}
	v := view{User: id, Nav: "onboarding", D: d}
	if d.Step == 1 || d.Step == d.Steps {
		s.withCapabilities(r, id, &v)
	}
	s.render(w, r, http.StatusOK, "onboarding.html", v)
}

// firstVisit sends a person who never saw the Onboarding there, once.
func (s *Server) firstVisit(w http.ResponseWriter, r *http.Request, id Identity) bool {
	if s.onboard == nil || r.URL.RawQuery != "" || r.Header.Get("HX-Request") == "true" {
		return false
	}
	seen, err := s.onboard.Seen(r.Context(), id.UID)
	if err != nil || seen {
		return false
	}
	if err := s.onboard.MarkSeen(r.Context(), id.UID); err != nil {
		return false // never loop: without a stored mark the start page stays reachable
	}
	http.Redirect(w, r, "/onboarding", http.StatusSeeOther)
	return true
}
