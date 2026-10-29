// SPDX-License-Identifier: MIT

package portal

import "net/http"

func (s *Server) restoreSession(w http.ResponseWriter, r *http.Request, id Identity) {
	project, ok := s.projectName(w, r)
	if !ok {
		return
	}
	if err := s.be.RestoreSession(r.Context(), id.UID, project, r.PathValue("session")); err != nil {
		s.serverError(w, r, err)
		return
	}
	navigate(w, r, "/projects/"+project)
}
