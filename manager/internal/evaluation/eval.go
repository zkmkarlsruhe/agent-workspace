// SPDX-License-Identifier: MIT

// Package evaluation evaluates the collected chats for the prototype review
// (/admin): one structured verdict per chat from the fast model, heuristic
// flags as fallback, a daily Markdown report and the read audit.
package evaluation

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Eval is the verdict on one chat; it is stored as JSON.
type Eval struct {
	Topic             string   `json:"topic"`
	Outcome           string   `json:"outcome"`
	Success           string   `json:"success"` // yes | no | partly; "" = unknown (heuristic)
	Problems          []string `json:"problems"`
	PrivateData       bool     `json:"private_data"`
	PrivateDataReason string   `json:"private_data_reason"`
	Abuse             bool     `json:"misuse"`
	AbuseReason       string   `json:"misuse_reason"`
	Improvements      []Idea   `json:"improvements"`
}

type Idea struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

var (
	successValues = []string{"yes", "no", "partly"}
	problemKinds  = []string{"tool-error", "misunderstanding", "aborted", "model-error"}
	ideaKinds     = []string{"missing-skill", "missing-tool", "unclear-prompt", "missing-guide", "other"}
)

const systemPrompt = `You evaluate a chat between a [[organization]] staff member and an AI helper, to improve a prototype.
Answer only with a JSON object, without explanation, with exactly these fields, the texts in German:
{"topic": "what it was about, at most 8 words",
 "outcome": "what came out, one sentence",
 "success": "yes" | "no" | "partly",
 "problems": list of "tool-error", "misunderstanding", "aborted", "model-error" (empty if none),
 "private_data": true if obviously private or confidential personal data was entered (health, addresses, salaries, passwords), else false,
 "private_data_reason": "short, without repeating the data" or "",
 "misuse": true on obvious misuse (harassment, crimes, getting around blocks), else false,
 "misuse_reason": "short" or "",
 "improvements": list of {"kind": "missing-skill" | "missing-tool" | "unclear-prompt" | "missing-guide" | "other", "text": "short"}, at most 3}
Lines "- Werkzeug ..." are the helper's tool calls with their result (fertig = done, Fehler = error, abgebrochen = aborted).`

// Parse reads the model answer: the first JSON object in it, validated.
// Unknown problem and idea kinds are dropped or become "other"; texts
// are cut. A missing topic or an invalid "success" is an error.
func Parse(answer string) (Eval, error) {
	var e Eval
	i, j := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if i < 0 || j < i {
		return e, errors.New("auswertung: no JSON object")
	}
	if err := json.Unmarshal([]byte(answer[i:j+1]), &e); err != nil {
		return e, err
	}
	e.Success = strings.ToLower(strings.TrimSpace(e.Success))
	if !slices.Contains(successValues, e.Success) {
		return e, errors.New("auswertung: invalid success")
	}
	if e.Topic = cut(e.Topic, 120); e.Topic == "" {
		return e, errors.New("auswertung: no topic")
	}
	e.Outcome = cut(e.Outcome, 300)
	e.PrivateDataReason, e.AbuseReason = cut(e.PrivateDataReason, 300), cut(e.AbuseReason, 300)
	var ps []string
	for _, p := range e.Problems {
		if p = strings.ToLower(strings.TrimSpace(p)); slices.Contains(problemKinds, p) && !slices.Contains(ps, p) {
			ps = append(ps, p)
		}
	}
	e.Problems = ps
	var is []Idea
	for _, it := range e.Improvements {
		if it.Text = cut(it.Text, 200); it.Text == "" || len(is) == 5 {
			continue
		}
		if it.Kind = strings.ToLower(strings.TrimSpace(it.Kind)); !slices.Contains(ideaKinds, it.Kind) {
			it.Kind = "other"
		}
		is = append(is, it)
	}
	e.Improvements = is
	return e, nil
}

// Heuristic is the verdict without a model: topic = title, problems from the counts.
func Heuristic(c store.Chat) Eval {
	return Eval{Topic: cut(c.Title, 120), Problems: measured(c, nil)}
}

// measured adds the problems the counts prove to ps.
func measured(c store.Chat, ps []string) []string {
	for _, m := range []struct {
		n int
		p string
	}{{c.ToolErrors, "tool-error"}, {c.Aborted, "aborted"}, {c.ModelErrors, "model-error"}} {
		if m.n > 0 && !slices.Contains(ps, m.p) {
			ps = append(ps, m.p)
		}
	}
	return ps
}

// cut collapses whitespace and cuts to n bytes at a rune boundary.
func cut(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + " ..."
}

// Flags are the filter marks of a chat in the admin list.
func Flags(c store.Chat, e *Eval) []string {
	var f []string
	if c.ToolErrors > 0 {
		f = append(f, "tool-error")
	}
	if c.Aborted > 0 {
		f = append(f, "aborted")
	}
	if c.ModelErrors > 0 {
		f = append(f, "model-error")
	}
	if e == nil {
		return append(f, "unevaluated")
	}
	if e.Success == "no" {
		f = append(f, "unsuccessful")
	}
	if e.PrivateData {
		f = append(f, "private-data")
	}
	if e.Abuse {
		f = append(f, "misuse")
	}
	return f
}

// AllFlags is the order of the flag filter.
var AllFlags = []string{"tool-error", "aborted", "model-error", "unsuccessful", "private-data", "misuse", "unevaluated"}
