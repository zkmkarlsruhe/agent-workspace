// SPDX-License-Identifier: MIT

package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// Facts are the facts about a person: name, e-mail and groups from the
// Authentik headers, the rest from the configured directory (empty if unknown).
type Facts struct {
	Name       string    `json:"name"`
	Email      string    `json:"email"`
	Department string    `json:"department,omitempty"`
	Role       string    `json:"role,omitempty"`
	Phone      string    `json:"phone,omitempty"`
	Room       string    `json:"room,omitempty"`
	Groups     []string  `json:"groups,omitempty"`
	AsOf       time.Time `json:"as_of"`
}

// AboutMe renders the facts section in lang; empty facts are left out.
func AboutMe(f Facts, lang string) string {
	var b strings.Builder
	b.WriteString(tx(lang, "## About me") + "\n")
	for _, kv := range [][2]string{{"Name", f.Name}, {"Email", f.Email}, {"Department", f.Department}, {"Role", f.Role},
		{"Phone", f.Phone}, {"Room", f.Room}, {"Groups", strings.Join(f.Groups, ", ")}} {
		if v := oneLine(kv[1]); v != "" {
			fmt.Fprintf(&b, "- %s: %s\n", tx(lang, kv[0]), v)
		}
	}
	if !f.AsOf.IsZero() {
		layout := "02.01.2006"
		if lang == LangEN {
			layout = "2 Jan 2006"
		}
		fmt.Fprintf(&b, tx(lang, "\n(As of %s, from the login and the %s directory.)\n"), f.AsOf.Format(layout), instance.Value("organization"))
	}
	return b.String()
}

// oneLine keeps a directory value from adding lines or markers to the block.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if !safeText(s) {
		return ""
	}
	return s
}

// BaseRules are the lines that come straight from the closed fields; the
// model never rewrites them, so it cannot flip the AddressForm. lang is the
// language of the block itself, a.Language the language of the answers.
func BaseRules(a Answers, lang string) []string {
	var out []string
	if a.Language == LanguageEnglish {
		out = append(out, "Answer in English.") // du/Sie has no English counterpart
	} else {
		if a.AddressForm == AddressFormal {
			out = append(out, "In German, address me as “Sie”.")
		} else {
			out = append(out, "In German, address me as “du”.")
		}
		out = append(out, "Answer in German.")
	}
	out = append(out, map[Tone]string{
		ToneFactual:  "Write factually and concisely, without filler phrases.",
		ToneFriendly: "Write in a friendly, approachable way.",
		ToneCasual:   "Write casually and directly.",
	}[a.Tone])
	out = append(out, map[Length]string{
		LengthShort:    "Keep answers short: the result first, explanations only when I ask.",
		LengthMedium:   "Answer at medium length: the result and a brief rationale.",
		LengthDetailed: "Explain in detail, with intermediate steps and background.",
	}[a.Length])
	if slices.Contains(a.Formats, FormatChat) {
		out = append(out, "Results preferably right in the chat; a file only when I ask for one.")
	} else {
		var ls []string
		for _, f := range a.Formats {
			ls = append(ls, label(f, Formats))
		}
		out = append(out, fmt.Sprintf(tx(lang, "If a file is produced, preferably as %s."), join(ls, tx(lang, " or "))))
	}
	for i := range out {
		out[i] = tx(lang, out[i])
	}
	return slices.DeleteFunc(out, func(s string) bool { return s == "" })
}

// Wishes are the template lines of the free fields (the fallback when the
// model is not available or answers nonsense).
func Wishes(a Answers, lang string) []string {
	var out []string
	if a.Field != "" {
		out = append(out, fmt.Sprintf(tx(lang, "My field: %s."), a.Field))
	}
	if len(a.Tasks) > 0 {
		var ls []string
		for _, t := range a.Tasks {
			ls = append(ls, tx(lang, taskText[t]))
		}
		out = append(out, fmt.Sprintf(tx(lang, "Typical tasks: %s."), strings.Join(ls, ", ")))
	}
	for _, l := range Lines(a.Always) {
		out = append(out, fmt.Sprintf(tx(lang, "Always: %s"), l))
	}
	for _, l := range Lines(a.Never) {
		out = append(out, fmt.Sprintf(tx(lang, "Never: %s"), l))
	}
	for _, l := range Lines(a.Note) {
		out = append(out, l)
	}
	return out
}

// taskText names the tasks in the block (schema.go labels are the form's).
var taskText = map[Task]string{TaskResearch: "Research", TaskSpreadsheets: "Analysing spreadsheets", TaskTexts: "Writing texts",
	TaskPresentations: "Presentations", TaskProofreading: "Proofreading", TaskTranslation: "Translating"}

func join(ls []string, or string) string {
	if len(ls) < 2 {
		return strings.Join(ls, "")
	}
	return strings.Join(ls[:len(ls)-1], ", ") + or + ls[len(ls)-1]
}

func bullets(ls []string) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString("- " + l + "\n")
	}
	return b.String()
}

// Template is the deterministic "So arbeite ich" text in lang.
func Template(a Answers, lang string) string {
	return bullets(append(BaseRules(a, lang), Wishes(a, lang)...))
}

// Complete sends one system and one user message to the fast model and
// returns the answer text.
type Complete func(ctx context.Context, system, user string) (string, error)

const llmSystem = `You write working instructions for an AI helper. A person has described how they work. Turn that into short, clear rules for the helper, in English, in the first person of that person (e.g. "Please always add a totals row to tables."). Invent nothing, leave nothing out, no repetitions, no salutation, no headings, no emojis. Answer only with JSON in exactly this form: {"rules": ["...", "..."]}. At most 12 rules, each at most 200 characters, one line.`

// Rules is the schema of the model's answer.
type Rules struct {
	Rules []string `json:"rules"`
}

var errAnswer = errors.New("profil: unusable model answer")

// ParseRules reads the model's JSON (also inside a code fence) and checks it
// against the schema: 1 to 12 single-line rules of at most 200 characters,
// no headings, no block markers.
func ParseRules(raw string) ([]string, error) {
	i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if i < 0 || j < i {
		return nil, errAnswer
	}
	var r Rules
	if err := json.Unmarshal([]byte(raw[i:j+1]), &r); err != nil {
		return nil, errors.Join(errAnswer, err)
	}
	if len(r.Rules) == 0 || len(r.Rules) > 12 {
		return nil, errAnswer
	}
	out := make([]string, 0, len(r.Rules))
	for _, l := range r.Rules {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-*"))
		if l == "" || strings.ContainsAny(l, "\n#") || utf8.RuneCountInString(l) > 200 || !safeText(l) {
			return nil, errAnswer
		}
		out = append(out, l)
	}
	return out, nil
}

// WorkingStyle builds the "So arbeite ich" text: the fixed lines from the
// closed fields, then the free fields rephrased by the model. Without a
// model, without free text or on any model error it falls back to the
// template; llm reports whether the model's text was used.
func WorkingStyle(ctx context.Context, complete Complete, a Answers, lang string) (text string, llm bool) {
	free := Wishes(a, lang)
	if complete == nil || len(free) == 0 {
		return Template(a, lang), false
	}
	in, _ := json.Marshal(map[string]any{"field": a.Field, "tasks": a.Tasks,
		"always": Lines(a.Always), "never": Lines(a.Never), "note": a.Note})
	raw, err := complete(ctx, tx(lang, llmSystem), string(in))
	if err != nil {
		return Template(a, lang), false
	}
	rs, err := ParseRules(raw)
	if err != nil {
		return Template(a, lang), false
	}
	return bullets(append(BaseRules(a, lang), rs...)), true
}

// CleanText normalises the edited text and checks it; "" = use the template.
func CleanText(s string) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	switch {
	case utf8.RuneCountInString(s) > MaxText:
		return "", fmt.Errorf("Bitte höchstens %d Zeichen.", MaxText)
	case !safeText(s):
		return "", errors.New("Dieser Text enthält Zeichen, die hier nicht gehen.")
	}
	// a heading of its own would end the managed section for the reader
	var ls []string
	for l := range strings.SplitSeq(s, "\n") {
		ls = append(ls, strings.TrimLeft(l, "#"))
	}
	if s = strings.Join(ls, "\n"); s != "" {
		s += "\n"
	}
	return s, nil
}
