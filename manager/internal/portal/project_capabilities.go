// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"net/http"
	"net/url"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// ProjectCapabilities is optional so installations without a catalog keep the existing form.
type ProjectCapabilities interface {
	ProjectCapabilities(context.Context, string, string) ([]Tool, error)
	SelectCapabilities(context.Context, string, []string) ([]string, error)
	SetProjectCapability(context.Context, string, string, string, bool) (ToolEffect, error)
}

func (s *Server) projectCapabilities() ProjectCapabilities {
	if !instance.Current().Capabilities.Projects {
		return nil
	}
	c, _ := s.tools.(ProjectCapabilities)
	return c
}

func (s *Server) setProjectCapability(w http.ResponseWriter, r *http.Request, id Identity) {
	c := s.projectCapabilities()
	if c == nil {
		http.NotFound(w, r)
		return
	}
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUp)
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	effect, err := c.SetProjectCapability(r.Context(), id.UID, name, r.FormValue("capability"), r.FormValue("on") == "1")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/projects/"+url.PathEscape(name)+"?effect="+effectQuery[effect]+"#capabilities", http.StatusSeeOther)
}
