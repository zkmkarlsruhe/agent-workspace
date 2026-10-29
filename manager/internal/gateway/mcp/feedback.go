// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// FeedbackStore is the part of store.Store feedback_send needs.
type FeedbackStore interface {
	AddFeedback(ctx context.Context, f store.Feedback, since time.Time, max int) (int64, error)
}

// Feedback limits, shared with the portal form (same table, same count).
const (
	FeedbackPerDay  = 10
	FeedbackMaxText = 4000 // runes
)

var feedbackKinds = []string{"problem", "idea", "praise", "question"}

const feedbackDesc = "Sends the person's feedback to the team running this service: a problem, an idea, praise or a question. " +
	"Only call it when the person explicitly wants it, and only after they have confirmed your summary. " +
	"The last messages of this chat (tool calls shortened), project and session are attached automatically; only that team reads them."

var feedbackTool = map[string]any{
	"name":        "feedback_send",
	"title":       "Feedback to the team",
	"description": feedbackDesc,
	"inputSchema": map[string]any{
		"type":     "object",
		"required": []string{"message"},
		"properties": map[string]any{
			"message":          map[string]any{"type": "string", "minLength": 1, "maxLength": FeedbackMaxText, "description": "what the person wants to tell the team, in their words or as a summary they confirmed"},
			"kind":             map[string]any{"type": "string", "enum": feedbackKinds, "default": "problem"},
			"context_messages": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "default": 6, "description": "how many of the last chat messages are attached"},
		},
	},
}

func (s *Server) feedbackSend(ctx context.Context, u store.User, session string, raw json.RawMessage) (toolResult, callMeta) {
	m := callMeta{capability: "feedback", target: "manager", status: http.StatusBadRequest}
	a := struct {
		Message  string `json:"message"`
		Kind     string `json:"kind"`
		Messages int    `json:"context_messages"`
	}{Kind: "problem", Messages: 6}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Message) == "" {
		return errorResult(ctx, "Invalid parameters for feedback_send: message is missing."), m
	}
	a.Message = strings.TrimSpace(a.Message)
	if utf8.RuneCountInString(a.Message) > FeedbackMaxText {
		return errorResult(ctx, "message is too long: at most %d characters. Summarise it more briefly.", FeedbackMaxText), m
	}
	if !slices.Contains(feedbackKinds, a.Kind) {
		return errorResult(ctx, "kind must be problem, idea, praise or question."), m
	}
	a.Messages = min(max(a.Messages, 1), 20)
	f := store.Feedback{At: time.Now(), UID: u.UID, Source: "chat", Kind: a.Kind, Text: a.Message, SessionID: session}
	if s.FeedbackContext != nil {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		fc, err := s.FeedbackContext(cctx, u.UID, session, a.Messages)
		cancel()
		if err != nil { // the person's words still reach the team, only without the excerpt
			s.Log.WarnContext(ctx, "feedback_send: context", "uid", u.UID, "session_id", session, "err", err)
		}
		f.Project, f.Version, f.Context = fc.Project, fc.Version, fc.Messages
		if fc.SessionID != "" {
			f.SessionID = fc.SessionID
		}
	}
	_, err := s.Feedback.AddFeedback(ctx, f, f.At.Add(-24*time.Hour), FeedbackPerDay)
	if errors.Is(err, store.ErrLimit) {
		m.status = http.StatusTooManyRequests
		return errorResult(ctx, "%d feedback messages have already been sent in the last 24 hours; more are possible later.", FeedbackPerDay), m
	}
	if err != nil {
		s.Log.ErrorContext(ctx, "feedback_send: store", "uid", u.UID, "err", err)
		m.status = http.StatusInternalServerError
		return errorResult(ctx, "The feedback could not be saved. Please try again later."), m
	}
	m.status = http.StatusOK
	if n := strings.Count(f.Context, "\n## "); n > 0 {
		return toolResult{Content: textResult(ctx, "Sent to the team running this service. The last %d messages of this chat are attached; only that team reads them.", n)}, m
	}
	return toolResult{Content: textResult(ctx, "Sent to the team running this service, without chat messages (they could not be read). Only that team reads it.")}, m
}
