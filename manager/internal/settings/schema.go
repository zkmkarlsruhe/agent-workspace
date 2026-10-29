// SPDX-License-Identifier: MIT

// Package settings keeps who a person is (Facts: Authentik + Workspace directory)
// and how they like to work (Answers: a short guided form), and turns both
// into a managed block of /home/user/.config/opencode/AGENTS.md.
//
// Answers is the typed schema in the spirit of a pydantic model: explicit
// fields, closed enums, defaults (Default) and one Validate that returns
// German messages per field.
package settings

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

type AddressForm string

const (
	AddressInformal AddressForm = "informal"
	AddressFormal   AddressForm = "formal"
)

type Language string

const (
	LanguageGerman  Language = "de"
	LanguageEnglish Language = "en"
)

type Tone string

const (
	ToneFactual  Tone = "factual"
	ToneFriendly Tone = "friendly"
	ToneCasual   Tone = "casual"
)

type Length string

const (
	LengthShort    Length = "short"
	LengthMedium   Length = "medium"
	LengthDetailed Length = "detailed"
)

// Format "chat" means "chat only" and excludes the file formats.
type Format string

const (
	FormatChat       Format = "chat"
	FormatPDF        Format = "pdf"
	FormatWord       Format = "word"
	FormatExcel      Format = "excel"
	FormatPowerPoint Format = "powerpoint"
)

type Task string

const (
	TaskResearch      Task = "research"
	TaskSpreadsheets  Task = "spreadsheets"
	TaskTexts         Task = "writing"
	TaskPresentations Task = "presentations"
	TaskProofreading  Task = "proofreading"
	TaskTranslation   Task = "translation"
)

// Option is one choice of a closed field, with its German label for the form.
type Option[T ~string] struct {
	Value T
	Label string
}

var (
	AddressForms = []Option[AddressForm]{{AddressInformal, "du"}, {AddressFormal, "Sie"}}
	Languages    = []Option[Language]{{LanguageGerman, "Deutsch"}, {LanguageEnglish, "Englisch"}}
	Tones        = []Option[Tone]{{ToneFactual, "sachlich und knapp"}, {ToneFriendly, "freundlich"}, {ToneCasual, "locker"}}
	Formats      = []Option[Format]{{FormatChat, "nur im Chat"}, {FormatPDF, "PDF"}, {FormatWord, "Word"}, {FormatExcel, "Excel"}, {FormatPowerPoint, "PowerPoint"}}
	Tasks        = []Option[Task]{{TaskResearch, "Recherche"}, {TaskSpreadsheets, "Tabellen auswerten"}, {TaskTexts, "Texte schreiben"},
		{TaskPresentations, "Präsentationen"}, {TaskProofreading, "Korrekturlesen"}, {TaskTranslation, "Übersetzen"}}
)

// Length limits in characters; together they keep the block far below the 8 KiB of AGENTS.md.
const (
	MaxField = 200
	MaxList  = 500
	MaxNote  = 1000
	MaxText  = 3000 // the edited "So arbeite ich" text
)

// Answers are the answers of the Settings form.
type Answers struct {
	AddressForm AddressForm `json:"address_form"`
	Language    Language    `json:"language"`
	Tone        Tone        `json:"tone"`
	Length      Length      `json:"length"`
	Formats     []Format    `json:"formats"`
	Field       string      `json:"field"`
	Tasks       []Task      `json:"tasks"`
	Always      string      `json:"always"` // one item per line
	Never       string      `json:"never"`  // one item per line
	Note        string      `json:"note"`
}

// Default is the settings of someone who never filled in the form: "du"
// (Workspace-internal), German, answers in the chat. The Field comes from the
// directory when known.
func Default(f Facts) Answers {
	field := f.Department
	if f.Role != "" {
		field = strings.TrimPrefix(field+", "+f.Role, ", ")
	}
	if utf8.RuneCountInString(field) > MaxField {
		field = ""
	}
	return Answers{AddressForm: AddressInformal, Language: LanguageGerman, Tone: ToneFactual, Length: LengthMedium,
		Formats: []Format{FormatChat}, Field: field}
}

type FieldError struct{ Field, Msg string }

func valid[T ~string](v T, opts []Option[T]) bool {
	return slices.ContainsFunc(opts, func(o Option[T]) bool { return o.Value == v })
}

func validSet[T ~string](vs []T, opts []Option[T]) bool {
	for i, v := range vs {
		if !valid(v, opts) || slices.Index(vs, v) != i {
			return false
		}
	}
	return true
}

// Clean trims the free text fields; call before Validate.
func (a *Answers) Clean() {
	for _, p := range []*string{&a.Field, &a.Always, &a.Never, &a.Note} {
		*p = strings.TrimSpace(strings.ReplaceAll(*p, "\r\n", "\n"))
	}
}

// Validate checks every field; nil = valid.
func (a Answers) Validate() []FieldError {
	var errs []FieldError
	add := func(f, m string) { errs = append(errs, FieldError{f, m}) }
	if !valid(a.AddressForm, AddressForms) {
		add("address_form", "Bitte wähle „du“ oder „Sie“.")
	}
	if !valid(a.Language, Languages) {
		add("lang", "Bitte wähle eine Sprache.")
	}
	if !valid(a.Tone, Tones) {
		add("tone", "Bitte wähle einen Tonfall.")
	}
	if !valid(a.Length, Lengths) {
		add("length", "Bitte wähle, wie ausführlich die Antworten sein sollen.")
	}
	switch {
	case len(a.Formats) == 0 || !validSet(a.Formats, Formats):
		add("formats", "Bitte wähle mindestens ein Format.")
	case slices.Contains(a.Formats, FormatChat) && len(a.Formats) > 1:
		add("formats", "„Nur im Chat“ geht nicht zusammen mit Dateiformaten.")
	}
	if !validSet(a.Tasks, Tasks) {
		add("tasks", "Bitte wähle nur Aufgaben aus der Liste.")
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"field", a.Field, MaxField}, {"always", a.Always, MaxList}, {"never", a.Never, MaxList}, {"note", a.Note, MaxNote}} {
		switch {
		case utf8.RuneCountInString(f.v) > f.max:
			add(f.name, fmt.Sprintf("Bitte höchstens %d Zeichen.", f.max))
		case !safeText(f.v):
			add(f.name, "Dieser Text enthält Zeichen, die hier nicht gehen.")
		}
	}
	if f := a.Field; strings.Contains(f, "\n") {
		add("field", "Bitte nur eine Zeile.")
	}
	return errs
}

// safeText rejects text that could break the managed block: its markers and
// control characters other than newline and tab.
func safeText(s string) bool {
	if strings.Contains(s, markerStem) || strings.Contains(s, "<!-- "+instance.Value("profile_marker")+":profil") || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\t' || r == 0x7f })
}

// Lines splits a list field into its non-empty trimmed lines.
func Lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-*")); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func label[T ~string](v T, opts []Option[T]) string {
	for _, o := range opts {
		if o.Value == v {
			return o.Label
		}
	}
	return string(v)
}
