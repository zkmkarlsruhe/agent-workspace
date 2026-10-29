// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"
)

// FeedbackMax bounds the chat excerpt attached to one feedback.
const FeedbackMax = 64 << 10

// FeedbackContext is what the manager itself attaches to a feedback from the
// chat; nothing of it comes from the tool arguments.
type FeedbackContext struct {
	Project, SessionID, Version string
	Messages                    string // the last n messages rendered like the export, tool calls one line each
}

// FeedbackContext reads the last n messages of session from uid's running
// container. Without a session id it takes a running one: the call comes
// from an agent that runs right now.
func (s *Service) FeedbackContext(ctx context.Context, uid, session string, n int) (FeedbackContext, error) {
	var fc FeedbackContext
	c, ok := s.Client(uid)
	if !ok {
		return fc, ErrNotRunning
	}
	if i, err := c.Info(ctx); err == nil {
		fc.Version = i.Version
	}
	if session == "" {
		active, err := c.Active(ctx)
		if err != nil || len(active) == 0 {
			return fc, err
		}
		// shortcut: with several agents running at once the pick is arbitrary; OpenCode 2.2.0 sends the id for session tool calls
		slices.Sort(active)
		session = active[0]
	}
	se, err := c.SessionGet(ctx, session)
	if err != nil {
		return fc, err
	}
	fc.SessionID = se.ID
	if p, ok := strings.CutPrefix(se.Location.Directory, s.ws+"/"); ok && ValidProject(p) {
		fc.Project = p
	}
	// system, synthetic and idle entries render empty: fetch more to fill n
	msgs, err := c.LastMessages(ctx, se.ID, min(3*n, 100))
	if err != nil {
		return fc, err
	}
	var out []string
	for i := len(msgs) - 1; i >= 0 && len(out) < n; i-- {
		if m := renderMessage(msgs[i], false); m != "" { // feedback goes to the operator in German
			out = append(out, cut(m, chatMsg))
		}
	}
	slices.Reverse(out)
	total := 0
	for i := len(out) - 1; i >= 0; i-- { // the newest messages matter most
		if total += len(out[i]); total > FeedbackMax {
			out = append([]string{tr(false, "[... older messages cut ...]\n")}, out[i+1:]...)
			break
		}
	}
	fc.Messages = strings.Join(out, "")
	return fc, nil
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + tr(false, "\n[... message cut ...]\n")
}
