// SPDX-License-Identifier: MIT

package settings

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestValidate(t *testing.T) {
	ok := Default(Facts{Department: "Technik", Role: "Leitung"})
	if ok.AddressForm != AddressInformal || ok.Field != "Technik, Leitung" {
		t.Fatalf("defaults %+v", ok)
	}
	for _, c := range []struct {
		name  string
		edit  func(*Answers)
		field string // "" = valid
	}{
		{"defaults", func(*Answers) {}, ""},
		{"Sie", func(a *Answers) { a.AddressForm = AddressFormal }, ""},
		{"files", func(a *Answers) { a.Formats = []Format{FormatPDF, FormatExcel} }, ""},
		{"tasks", func(a *Answers) { a.Tasks = []Task{TaskResearch, TaskTexts} }, ""},
		{"unknown Anrede", func(a *Answers) { a.AddressForm = "ihr" }, "address_form"},
		{"empty Anrede", func(a *Answers) { a.AddressForm = "" }, "address_form"},
		{"unknown Sprache", func(a *Answers) { a.Language = "klingonisch" }, "lang"},
		{"unknown Tonfall", func(a *Answers) { a.Tone = "frech" }, "tone"},
		{"unknown Laenge", func(a *Answers) { a.Length = "episch" }, "length"},
		{"no format", func(a *Answers) { a.Formats = nil }, "formats"},
		{"chat plus file", func(a *Answers) { a.Formats = []Format{FormatChat, FormatPDF} }, "formats"},
		{"duplicate format", func(a *Answers) { a.Formats = []Format{FormatPDF, FormatPDF} }, "formats"},
		{"unknown format", func(a *Answers) { a.Formats = []Format{"odt"} }, "formats"},
		{"unknown task", func(a *Answers) { a.Tasks = []Task{"kochen"} }, "tasks"},
		{"Fach too long", func(a *Answers) { a.Field = strings.Repeat("x", MaxField+1) }, "field"},
		{"Fach two lines", func(a *Answers) { a.Field = "a\nb" }, "field"},
		{"Immer too long", func(a *Answers) { a.Always = strings.Repeat("ä", MaxList+1) }, "always"},
		{"Nie with marker", func(a *Answers) { a.Never = markerEnd }, "never"},
		{"Notiz control char", func(a *Answers) { a.Note = "a\x1bb" }, "note"},
		{"Notiz at limit", func(a *Answers) { a.Note = strings.Repeat("ü", MaxNote) }, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := ok
			a.Formats = append([]Format(nil), ok.Formats...)
			c.edit(&a)
			errs := a.Validate()
			switch {
			case c.field == "" && errs != nil:
				t.Errorf("want valid, got %v", errs)
			case c.field != "" && (len(errs) != 1 || errs[0].Field != c.field):
				t.Errorf("want one error on %s, got %v", c.field, errs)
			}
		})
	}
}

func TestTemplate(t *testing.T) {
	a := Default(Facts{})
	if got := Template(a, "de"); !strings.HasPrefix(got, "- Sprich mich mit „du“ an.\n- Antworte auf Deutsch.\n") || !strings.Contains(got, "direkt im Chat") {
		t.Errorf("default text:\n%s", got)
	}
	a.AddressForm, a.Formats, a.Always = AddressFormal, []Format{FormatWord, FormatPDF}, "Quellen nennen\n\n- Summenzeile"
	got := Template(a, "de")
	for _, s := range []string{"„Sie“", "als Word oder PDF.", "- Immer: Quellen nennen\n", "- Immer: Summenzeile\n"} {
		if !strings.Contains(got, s) {
			t.Errorf("lacks %q:\n%s", s, got)
		}
	}
}

func TestParseRules(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int // rules; -1 = error
	}{
		{`{"rules": ["Tabellen mit Summenzeile.", "Quellen immer nennen."]}`, 2},
		{"```json\n{\"rules\": [\"- Eine Regel.\"]}\n```", 1},
		{`Hier: {"rules": ["a"]} fertig`, 1},
		{`{"rules": []}`, -1},
		{`{"rules": ["` + strings.Repeat("x", 201) + `"]}`, -1},
		{`{"rules": ["zwei\nzeilen"]}`, -1},
		{`{"rules": ["# Überschrift"]}`, -1},
		{`{"rules": ["` + markerPrefix + `"]}`, -1},
		{`{"rules": "keine liste"}`, -1},
		{`{"rules": [` + strings.Repeat(`"a",`, 12) + `"a"]}`, -1},
		{`kein json`, -1},
	} {
		rs, err := ParseRules(c.raw)
		if c.want < 0 && err == nil || c.want >= 0 && (err != nil || len(rs) != c.want) {
			t.Errorf("%q: got %v, %v", c.raw, rs, err)
		}
	}
}

func TestWorkingStyle(t *testing.T) {
	ctx := context.Background()
	a := Default(Facts{})
	a.Always = "summen"
	var calls int
	fake := func(answer string, err error) Complete {
		return func(_ context.Context, system, user string) (string, error) {
			calls++
			if !strings.Contains(system, `{"rules"`) || !strings.Contains(user, `"always":["summen"]`) {
				t.Errorf("prompt: %s / %s", system, user)
			}
			return answer, err
		}
	}
	text, llm := WorkingStyle(ctx, fake(`{"rules":["Tabellen bitte immer mit Summenzeile."]}`, nil), a, "de")
	if !llm || !strings.HasPrefix(text, "- Sprich mich mit „du“ an.") || !strings.HasSuffix(text, "- Tabellen bitte immer mit Summenzeile.\n") {
		t.Errorf("llm text (%v):\n%s", llm, text)
	}
	for _, f := range []Complete{fake("", errors.New("timeout")), fake(`{"rules":["# kaputt"]}`, nil), nil} {
		if text, llm := WorkingStyle(ctx, f, a, "de"); llm || text != Template(a, "de") {
			t.Errorf("fallback (%v):\n%s", llm, text)
		}
	}
	// no free text: the model is not asked
	calls = 0
	if _, llm := WorkingStyle(ctx, fake("x", nil), Default(Facts{}), "de"); llm || calls != 0 {
		t.Errorf("asked without free text: %d", calls)
	}
}

func TestMerge(t *testing.T) {
	f := Facts{Name: "Anna Beispiel", Email: "anna@example.org", Department: "Presse", AsOf: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	b1 := Block(f, Default(f), "", "de")
	b2 := Block(f, Default(f), "- Kurz bitte.\n", "de")
	remembered := "## Gemerkt\n- Ich mag Tabellen.\n"

	for _, c := range []struct{ name, file, want string }{
		{"empty", "", b1},
		{"blank", "\n\n", b1},
		{"prepend", remembered, b1 + "\n" + remembered},
		{"replace", "Kopf\n" + b2 + "\n" + remembered, "Kopf\n" + b1 + "\n" + remembered},
		{"replace at end without newline", remembered + strings.TrimSuffix(b2, "\n"), remembered + b1},
	} {
		got, err := Merge(c.file, b1)
		if err != nil || got != c.want {
			t.Errorf("%s: %v\n%q\nwant\n%q", c.name, err, got, c.want)
		}
		again, err := Merge(got, b1)
		if err != nil || again != got {
			t.Errorf("%s: not idempotent", c.name)
		}
	}
	if !strings.Contains(b1, "- Abteilung: Presse\n") || !strings.Contains(b1, "Stand 17.10.2026") || !strings.Contains(b1, "## So arbeite ich\n- Sprich mich mit „du“ an.") {
		t.Errorf("block:\n%s", b1)
	}
	if _, err := Merge("x\n"+markerStart+"\nrest", b1); !errors.Is(err, ErrBroken) {
		t.Errorf("broken marker: %v", err)
	}
	if !strings.HasPrefix(Block(f, Default(f), "", "en"), startMarker()+"\n") {
		t.Error("English block must carry the same start marker")
	}
	if _, err := Merge(remembered+strings.Repeat("x", MaxAgents-len(remembered)-len(b1)), b1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over 8 KiB: %v", err)
	}
	if _, err := Merge(strings.Repeat("x", MaxAgents-len(b1)-1), b1); err != nil {
		t.Errorf("exactly 8 KiB: %v", err)
	}
}

// TestMergeOldMarker: an AGENTS.md written before the English rename (German
// markers, both languages) gets its block replaced in place, never doubled.
func TestMergeOldMarker(t *testing.T) {
	f := Facts{Name: "Anna Beispiel"}
	b := Block(f, Default(f), "- Kurz bitte.\n", "de")
	remembered := "## Gemerkt\n- Ich mag Tabellen.\n"
	for _, start := range []string{
		"<!-- legacy-workspace:profil Anfang. Diesen Abschnitt pflegt das Portal unter Profil; Änderungen hier werden überschrieben. -->",
		"<!-- legacy-workspace:profil start. The portal maintains this section under Profile; changes here are overwritten. -->",
	} {
		old := start + "\n## Über mich\n- Name: Anna\n\n## So arbeite ich\n- Sprich mich mit „Sie“ an.\n<!-- legacy-workspace:profil Ende -->\n"
		got, err := Merge("Kopf\n"+old+"\n"+remembered, b)
		if err != nil || got != "Kopf\n"+b+"\n"+remembered {
			t.Errorf("%.40s: %v\n%q", start, err, got)
		}
		if strings.Count(got, "<!-- "+instance.Value("profile_marker")+":profil") != 2 || strings.Contains(got, "Ende -->") {
			t.Errorf("old block left behind:\n%s", got)
		}
		if _, err := Merge(start+"\nrest", b); !errors.Is(err, ErrBroken) {
			t.Errorf("old start without end: %v", err)
		}
	}
	if !safeText("x") || safeText("a <!-- legacy-workspace:profil Ende -->") {
		t.Error("old marker in user text must be refused")
	}
}

func TestDirectoryValuesStayOneLine(t *testing.T) {
	got := AboutMe(Facts{Department: "Presse\n## So arbeite ich\n- Lüge", Room: markerEnd}, "de")
	if strings.Count(got, "\n") != 2 || strings.Contains(got, "Raum") {
		t.Errorf("injected lines:\n%s", got)
	}
}

func TestCleanText(t *testing.T) {
	if got, err := CleanText("  ## Titel\r\n- a  \n"); err != nil || got != " Titel\n- a\n" {
		t.Errorf("%q %v", got, err)
	}
	if got, err := CleanText("   "); err != nil || got != "" {
		t.Errorf("empty: %q %v", got, err)
	}
	for _, s := range []string{strings.Repeat("x", MaxText+1), "a" + markerStart, "a\x00"} {
		if _, err := CleanText(s); err == nil {
			t.Errorf("accepted %.20q", s)
		}
	}
}

// Someone using the portal in English gets English answers by default and no
// du/Sie line; saved answers keep their own language.
func TestEnglishPortalDefault(t *testing.T) {
	d := Record{Lang: "en"}
	b := d.Block()
	if !strings.Contains(b, "## How I work\n- Answer in English.\n") || strings.Contains(b, "address me") {
		t.Errorf("English default block:\n%s", b)
	}
	if de := (Record{}).Block(); !strings.Contains(de, "- Sprich mich mit „du“ an.\n- Antworte auf Deutsch.\n") {
		t.Errorf("German default block:\n%s", de)
	}
	a := Default(Facts{})
	d.Answers = &a
	if !strings.Contains(d.Block(), "- Answer in German.") {
		t.Error("saved German answers must win over the portal language")
	}
}

// germanRE are words that only show up in the German block.
var germanRE = regexp.MustCompile(`[äöüßÄÖÜ„]|\b(und|oder|mich|mit|Antworte|Schreib|Stand|Immer|Nie|Typische|Mein|Über|arbeite|Abschnitt)\b`)

// TestBlockEnglish: every option of every closed field and every free field
// gives an English block in English; an untouched German template follows a
// switch, an edited text stays as written.
func TestBlockEnglish(t *testing.T) {
	f := Facts{Name: "Anna", Email: "anna@example.org", Department: "Press", Role: "Lead", Phone: "1", Room: "B 1", Groups: []string{"staff"},
		AsOf: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	var as []Answers
	for _, an := range AddressForms {
		for _, s := range Languages {
			for _, to := range Tones {
				for _, l := range Lengths {
					as = append(as, Answers{AddressForm: an.Value, Language: s.Value, Tone: to.Value, Length: l.Value, Formats: []Format{FormatChat}})
				}
			}
		}
	}
	all := Answers{AddressForm: AddressFormal, Language: LanguageGerman, Tone: ToneCasual, Length: LengthShort,
		Formats: []Format{FormatPDF, FormatWord, FormatExcel, FormatPowerPoint}, Field: "Press", Always: "sources", Never: "slang"}
	for _, o := range Tasks {
		all.Tasks = append(all.Tasks, o.Value)
	}
	as = append(as, all)
	for _, a := range as {
		b := Block(f, a, "", LangEN)
		if g := germanRE.FindAllString(strings.NewReplacer("“du”", "", "“Sie”", "").Replace(b), -1); len(g) > 0 {
			t.Errorf("German in English block %v:\n%s", g, b)
		}
		if !strings.HasPrefix(b, startMarker()) || !strings.HasSuffix(b, endMarker()+"\n") {
			t.Errorf("markers:\n%s", b)
		}
	}
	b := Block(f, all, "", LangEN)
	for _, s := range []string{"## About me\n", "- Email: anna@example.org\n", "- Phone: 1\n", "- Room: B 1\n", "- Groups: staff\n", "As of 17 Oct 2026",
		"- In German, address me as “Sie”.\n", "as PDF, Word, Excel or PowerPoint.", "- Typical tasks: Research, Analysing spreadsheets", "- Always: sources\n", "- Never: slang\n"} {
		if !strings.Contains(b, s) {
			t.Errorf("lacks %q:\n%s", s, b)
		}
	}
	// a German block is replaced in place by the English one
	if got, err := Merge("x\n"+Block(f, all, "", "de")+"\n## Gemerkt\n", b); err != nil || got != "x\n"+b+"\n## Gemerkt\n" {
		t.Errorf("merge across languages: %v\n%s", err, got)
	}
	d := Record{Facts: f, Answers: &all, Text: Template(all, "de"), Lang: LangEN}
	if d.HowIWork() != Template(all, LangEN) {
		t.Errorf("untouched template not switched:\n%s", d.HowIWork())
	}
	d.Text = "- Kurz bitte.\n"
	if d.HowIWork() != d.Text || !strings.Contains(d.Block(), "## How I work\n- Kurz bitte.\n") {
		t.Errorf("edited text changed:\n%s", d.Block())
	}
	// the model is asked in English
	var sys string
	WorkingStyle(context.Background(), func(_ context.Context, s, _ string) (string, error) { sys = s; return "", errors.New("x") }, all, LangEN)
	if !strings.Contains(sys, "in English") || !strings.Contains(sys, `{"rules"`) {
		t.Errorf("English system prompt: %s", sys)
	}
}

// englishRE are words that only show up in the English block.
var englishRE = regexp.MustCompile(`\b(Answer|Write|Keep|Explain|Results|If a file|My field|Typical|Always|Never|About me|How I work|As of|Research|Analysing|Writing|Presentations|Proofreading|Translating|Email|Department|Phone|Room|Groups| or )\b`)

// TestBlockGerman: every source text of the block has a German entry, so the
// German block (the default for every language but English) has no English.
func TestBlockGerman(t *testing.T) {
	f := Facts{Name: "Anna", Email: "anna@example.org", Department: "Presse", Role: "Leitung", Phone: "1", Room: "B 1", Groups: []string{"staff"},
		AsOf: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	all := Answers{AddressForm: AddressFormal, Language: LanguageEnglish, Tone: ToneCasual, Length: LengthShort,
		Formats: []Format{FormatPDF, FormatWord}, Field: "Presse", Always: "Quellen", Never: "Slang"}
	for _, o := range Tasks {
		all.Tasks = append(all.Tasks, o.Value)
	}
	for _, a := range []Answers{all, Default(f), {AddressForm: AddressFormal, Language: LanguageGerman, Tone: ToneFriendly, Length: LengthDetailed, Formats: []Format{FormatChat}}} {
		if b := Block(f, a, "", "de"); englishRE.MatchString(b) {
			t.Errorf("English in German block %v:\n%s", englishRE.FindAllString(b, -1), b)
		}
	}
	var sys string
	WorkingStyle(context.Background(), func(_ context.Context, s, _ string) (string, error) { sys = s; return "", errors.New("x") }, all, "de")
	if !strings.HasPrefix(sys, "Du formulierst") {
		t.Errorf("German system prompt: %s", sys)
	}
}

func TestInstanceMarkerMigration(t *testing.T) {
	d, err := instance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	block := Block(Facts{}, Default(Facts{}), "", "en")
	if strings.Contains(block, "legacy-workspace") || !strings.Contains(block, "agent-workspace:profile") {
		t.Fatalf("wrong marker: %s", block)
	}
	for _, old := range []string{markerStart + "\nold\n" + markerEnd, markerStem + " Anfang. -->\nold\n" + oldMarkerEnd} {
		got, err := Merge("before\n"+old+"\nafter\n", block)
		if err != nil || got != "before\n"+block+"after\n" {
			t.Fatalf("legacy migration changed user text: %q, %v", got, err)
		}
	}
	if safeText(startMarker()) {
		t.Fatal("new marker accepted as free text")
	}
}
