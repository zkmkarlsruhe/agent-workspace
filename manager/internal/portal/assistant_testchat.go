// SPDX-License-Identifier: MIT

package portal

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistantchat"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type assistantTestData struct {
	Definition              assistants.Definition
	Hash                    string
	History                 string
	Messages                []assistantchat.Message
	Fixtures                map[string]string
	Groups, AvailableGroups []string
	Sources                 []string
	Error                   string
}

func (s *Server) testAssistant(w http.ResponseWriter, r *http.Request, id Identity) {
	v, err := s.savedAssistant(w, r)
	if err != nil {
		s.editorError(w, r, err)
		return
	}
	hash := instance.Current().AssistantPin(v).Hash
	d := assistantTestData{Definition: v, Hash: hash, History: "[]", Fixtures: map[string]string{}, Sources: v.KnowledgeSources, AvailableGroups: id.Groups}
	status := 200
	if r.PostForm.Get("send") == "1" {
		d.Groups = r.PostForm["groups"]
		for _, group := range d.Groups {
			if !slices.Contains(id.Groups, group) {
				s.editorError(w, r, assistantchat.ErrDenied)
				return
			}
		}
		if r.PostForm.Get("test_consent") != "true" {
			s.editorError(w, r, assistantchat.ErrDenied)
			return
		}
		for _, source := range d.Sources {
			d.Fixtures[source] = r.PostForm.Get("fixture_" + source)
		}
		err = json.Unmarshal([]byte(r.PostForm.Get("history")), &d.Messages)
		if err == nil {
			var messages []assistantchat.Message
			messages, err = s.taskAssistants.Preview(r.Context(), id.UID, d.Groups, v, d.Messages, r.PostForm.Get("question"), d.Fixtures)
			if err == nil {
				d.Messages = messages
			}
		}
		current := instance.Current()
		saved, ok := current.Assistant(v.ID)
		if !ok || current.AssistantPin(saved).Hash != hash {
			s.editorError(w, r, store.ErrAssistantConflict)
			return
		}
		action := "assistant_test_ok"
		if err != nil {
			action = "assistant_test_failed"
			status = 422
			d.Error = err.Error()
		}
		if auditErr := s.taskAssistants.Store.AuditAssistant(r.Context(), id.UID, v.ID, v.Version, hash, action); auditErr != nil {
			s.serverError(w, r, auditErr)
			return
		}
		b, _ := json.Marshal(d.Messages)
		d.History = string(b)
	}
	s.render(w, r, status, "assistant-test.html", view{User: id, Nav: "assistant-admin", D: d})
}
