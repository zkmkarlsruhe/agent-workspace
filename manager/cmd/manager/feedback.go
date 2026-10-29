// SPDX-License-Identifier: MIT

// Glue between the feedback table and the portal (form, /admin/feedback).
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/evaluation"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// portalFeedback is set by run(); nil = no form, no /admin/feedback.
var portalFeedback portal.Feedback

var _ portal.Feedback = feedback{}

// No mail, apprise or ntfy hook exists in the manager: alice sees new feedback under /admin/feedback only.

// feedback shares the limit of the chat tool feedback_send: one count per person.
type feedback struct {
	st  *store.Store
	log *slog.Logger
}

func (f feedback) Send(ctx context.Context, uid, project, kind, text string) error {
	now := time.Now()
	_, err := f.st.AddFeedback(ctx, store.Feedback{At: now, UID: uid, Source: "portal", Kind: kind, Text: text, Project: project},
		now.Add(-24*time.Hour), mcp.FeedbackPerDay)
	return err
}

func (f feedback) List(ctx context.Context, n int) ([]store.Feedback, error) {
	return f.st.FeedbackList(ctx, n)
}

func (f feedback) Get(ctx context.Context, r evaluation.Reader, id int64) (store.Feedback, error) {
	fb, err := f.st.FeedbackByID(ctx, id)
	if err != nil {
		return fb, err
	}
	// recorded before anything is shown, like a chat read
	if err := f.st.AuditFeedbackRead(ctx, time.Now(), r.UID, r.Username, id); err != nil {
		return store.Feedback{}, err
	}
	f.log.InfoContext(ctx, "admin feedback read", "admin", r.Username, "admin_uid", r.UID, "feedback", id, "person_uid", fb.UID)
	return fb, nil
}

func (f feedback) SetDone(ctx context.Context, id int64, done bool) error {
	return f.st.SetFeedbackDone(ctx, id, done)
}
