// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// KnowledgeSession admits general root sessions only. Child and assistant
// sessions do not inherit knowledge-write authority from a selected project.
func (s *Service) KnowledgeSession(ctx context.Context, u store.User, session string) (string, error) {
	current, err := s.st.UserByUID(ctx, u.UID)
	if err != nil {
		return "", err
	}
	if current.Incarnation != u.Incarnation || current.Generation != u.Generation {
		return "", ErrNotFound
	}
	c, ok := s.Client(u.UID)
	if !ok {
		return "", ErrNotRunning
	}
	se, err := c.SessionGet(ctx, session)
	if err != nil {
		return "", err
	}
	if se.ID != session || se.ParentID != "" || strings.HasPrefix(se.Agent, "profile-") {
		return "", ErrInvalid
	}
	folder, ok := strings.CutPrefix(se.Location.Directory, s.ws+"/")
	if !ok || !ValidProject(folder) {
		return "", ErrInvalid
	}
	p, err := s.st.ProjectByFolder(ctx, u.UID, folder)
	if err != nil {
		return "", err
	}
	if p.Assistant.ID != "" || p.Isolated {
		return "", ErrInvalid
	}
	return p.ID, nil
}
