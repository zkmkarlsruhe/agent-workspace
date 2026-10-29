// SPDX-License-Identifier: MIT

package portal

import (
	"net/http"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func (s *Server) teamInvitation(w http.ResponseWriter, r *http.Request, id Identity) {
	a, ok := s.knowledgeActor(w, r, id, true)
	if !ok {
		return
	}
	decision := r.PostForm.Get("decision")
	if decision != "accept" && decision != "decline" {
		s.knowledgeError(w, r, store.ErrKnowledgeInvalid)
		return
	}
	if err := s.knowledge.RespondTeamInvitation(r.Context(), a, r.PathValue("team"), decision == "accept", knowledgeRevision(r)); err != nil {
		s.knowledgeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/teams", http.StatusSeeOther)
}
