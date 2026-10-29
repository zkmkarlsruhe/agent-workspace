// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// Consent stores the acceptance of the prototype notice (internal/consent).
// The router lets a person without consent reach only the notice and a few
// pages; nil = no notice pages (tests of other pages).
type Consent interface {
	// Accepted returns when uid last accepted the notice and whether that was
	// the current version; zero time = never.
	Accepted(ctx context.Context, uid string) (at time.Time, current bool, err error)
	Accept(ctx context.Context, uid string) error
}

func init() {
	pageFiles = append(pageFiles, "consent.html", "consent-declined.html", "how-it-works.html")
}

func (s *Server) consentRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	page("GET /help", s.withConsentDate("help.html"))
	page("GET /privacy", s.withConsentDate("privacy.html"))
	page("GET /how-it-works", s.withConsentDate("how-it-works.html"))
	if s.consent == nil {
		return
	}
	page("GET /consent", s.consentPage)
	page("POST /consent", s.consentAccept)
	page("GET /consent/declined", s.declined)
}

type consentPageData struct {
	Next      string
	Consented string // "17.10.2026 um 14:03" / "17 Oct 2026 at 14:03", empty = no consent to this version
	Previous  bool   // accepted an older version, so the account and its data exist
}

// consentData looks up the consent; a failure only hides the date.
func (s *Server) consentData(r *http.Request, uid string) consentPageData {
	var d consentPageData
	if s.consent == nil {
		return d
	}
	at, current, err := s.consent.Accepted(r.Context(), uid)
	if err != nil {
		slog.WarnContext(r.Context(), "portal: consent", "err", err)
		return d
	}
	if current {
		d.Consented = timeAt(lang(r), at.Local())
	}
	d.Previous = !current && !noTime(at)
	return d
}

func (s *Server) withConsentDate(page string) func(http.ResponseWriter, *http.Request, Identity) {
	nav := strings.TrimSuffix(page, ".html")
	return func(w http.ResponseWriter, r *http.Request, id Identity) {
		v := view{User: id, Nav: nav, D: s.consentData(r, id.UID),
			Notice: feedbackNotice(r.URL.Query().Get("feedback"))}
		if page == "help.html" || page == "how-it-works.html" {
			s.withCapabilities(r, id, &v)
		}
		s.render(w, r, http.StatusOK, page, v)
	}
}

func (s *Server) withCapabilities(r *http.Request, id Identity, v *view) {
	v.Capabilities = map[string]bool{}
	v.CapabilityCopy = map[string]string{}
	d := instance.Current()
	if s.tools != nil {
		if tools, err := s.tools.Tools(r.Context(), id.UID); err == nil {
			v.ProjectScoped = tools.ProjectScoped
			for _, tool := range append(localize(lang(r), tools.Standard), localize(lang(r), tools.Catalog)...) {
				v.Capabilities[tool.ID] = true
				v.CapabilityCopy[tool.ID] = tool.Description
			}
		}
	}
	for _, tool := range []string{"image_view", "image_generate"} {
		v.Capabilities[tool] = d.ToolEnabled(tool) && s.capabilities[tool]
	}
	v.Capabilities["feedback_send"] = d.ToolEnabled("feedback_send") && s.feedback != nil
	v.Capabilities["image_edit"] = v.Capabilities["image_generate"] && s.capabilities["image_edit"]
	v.Capabilities["pdf"] = d.ToolEnabled("md_to_pdf") && s.pdf != nil
	v.Capabilities["web"] = v.Capabilities["web-research"] && d.ToolEnabled("web_search") && s.capabilities["web"]
	v.Capabilities["md-to-pdf"] = v.Capabilities["md-to-pdf"] && v.Capabilities["pdf"]
	v.Capabilities["rooms"] = v.Capabilities["rooms"] && d.Enabled("workspace-rooms") && s.capabilities["rooms"]
	v.Capabilities["workspace-knowledge"] = v.Capabilities["workspace-knowledge"] && d.Enabled("workspace-knowledge")
	v.Capabilities["library"] = v.Capabilities["library"] && d.Enabled("library")
	v.Capabilities["media"] = v.Capabilities["media"] && d.Enabled("toolbox")
	v.Capabilities["guides"] = true
	for _, id := range []string{"minutes", "letter-email", "press-release", "translate", "plain-language", "alt-text", "presentation", "event-planning", "travel-expenses"} {
		v.Capabilities["guides"] = v.Capabilities["guides"] && v.Capabilities[id]
	}
	for key, catalogID := range map[string]string{"web": "web-research", "pdf": "md-to-pdf", "knowledge": "workspace-knowledge", "library": "library"} {
		v.CapabilityCopy[key] = v.CapabilityCopy[catalogID]
		if copy := d.CopyText("capability_"+key, lang(r)); copy != "" {
			v.CapabilityCopy[key] = copy
		}
	}
	v.Capabilities["office"] = v.Capabilities["docx"] && v.Capabilities["xlsx"] && v.Capabilities["charts"] && v.Capabilities["pdf"]
}

func (s *Server) consentPage(w http.ResponseWriter, r *http.Request, id Identity) {
	d := s.consentData(r, id.UID)
	d.Next = s.next(r.URL.Query().Get("next"))
	s.render(w, r, http.StatusOK, "consent.html", view{User: id, Nav: "consent", D: d})
}

func (s *Server) consentAccept(w http.ResponseWriter, r *http.Request, id Identity) {
	d := s.consentData(r, id.UID)
	d.Next = s.next(r.FormValue("next"))
	if r.FormValue("agree") != "yes" {
		s.render(w, r, http.StatusUnprocessableEntity, "consent.html", view{User: id, Nav: "consent", D: d,
			Errors: []fieldError{{Field: "agree", Msg: "Please confirm that you have read the notice and agree."}}})
		return
	}
	if err := s.consent.Accept(r.Context(), id.UID); err != nil {
		s.serverError(w, r, err)
		return
	}
	navigate(w, r, d.Next)
}

func (s *Server) declined(w http.ResponseWriter, r *http.Request, id Identity) {
	s.render(w, r, http.StatusOK, "consent-declined.html", view{User: id, Nav: "consent", D: s.consentData(r, id.UID)})
}

// weiter keeps only a path on the portal or an address on a person host
// <slug>.<domain>; the router redirects a foreign slug back to the portal.
func (s *Server) next(v string) string {
	// browsers drop tabs and newlines and read a backslash as a slash: "/\t/x" is "//x"
	if strings.HasPrefix(v, "/consent") || strings.ContainsFunc(v, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' }) {
		return "/"
	}
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		return v
	}
	host, _ := strings.CutPrefix(s.origin, "https://")
	u, err := url.Parse(v)
	if err != nil || host == "" || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	if sub, ok := strings.CutSuffix(u.Host, "."+host); ok && sub != "" && !strings.ContainsAny(sub, ".:") {
		return u.String()
	}
	return "/"
}
