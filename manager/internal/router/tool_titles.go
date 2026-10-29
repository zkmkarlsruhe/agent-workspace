// SPDX-License-Identifier: MIT

package router

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

var titleSession = regexp.MustCompile(`^ses_[0-9A-Za-z]{8,80}$`)

func brandToolTitlesPath() string { return brandPrefix() + "tool-titles" }

// Called only after identity, consent, runtime access and generation binding.
func (rt *Router) serveToolTitles(w http.ResponseWriter, r *http.Request, u store.User) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	session := r.URL.Query().Get("session")
	if !titleSession.MatchString(session) {
		http.Error(w, "Invalid session", http.StatusBadRequest)
		return
	}
	titles := map[string]string{}
	if rt.ToolTitles != nil {
		var err error
		titles, err = rt.ToolTitles(r.Context(), u, session)
		if err != nil || r.Context().Err() != nil {
			http.Error(w, "Tool labels unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(titles)
}
