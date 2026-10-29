// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
)

// Settings is the core side of the Settings page (internal/settings); nil = no page.
type Settings interface {
	// Load returns the stored settings with facts refreshed if stale.
	Load(ctx context.Context, id Identity) (settings.Record, error)
	// Suggest turns valid answers into the "So arbeite ich" text; llm
	// reports whether the fast model wrote it (else the template did).
	Suggest(ctx context.Context, id Identity, a settings.Answers) (text string, llm bool)
	// Save stores and writes the block; settings.ErrTooLarge when it does not fit.
	Save(ctx context.Context, id Identity, a settings.Answers, text string) error
}

func init() {
	pageFiles = append(pageFiles, "settings.html")
}

func (s *Server) settingsRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.settings == nil {
		return
	}
	page("GET /settings", s.settingsPage)
	page("POST /settings", s.settingsSubmit)
	// Existing forms may still submit here during the transition.
	page("POST /profile", s.settingsSubmit)
}

type settingsData struct {
	Facts        settings.Facts
	A            settings.Answers
	Text         string
	Suggestion   bool // Text is a fresh, unsaved proposal
	LLM          bool
	Onboarding   bool
	AddressForms []settings.Option[settings.AddressForm]
	Languages    []settings.Option[settings.Language]
	Tones        []settings.Option[settings.Tone]
	Lengths      []settings.Option[settings.Length]
	Formats      []settings.Option[settings.Format]
	Tasks        []settings.Option[settings.Task]
	Block        string // what the helper reads, for "So sieht es der Helfer"
}

func newSettingsData(d settings.Record, a settings.Answers, text string) settingsData {
	if text == "" {
		text = settings.Template(a, d.Lang)
	}
	return settingsData{Facts: d.Facts, A: a, Text: text,
		AddressForms: settings.AddressForms, Languages: settings.Languages, Tones: settings.Tones, Lengths: settings.Lengths,
		Formats: settings.Formats, Tasks: settings.Tasks, Block: settings.Block(d.Facts, a, text, d.Lang)}
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, id Identity) {
	d, err := s.settings.Load(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pd := newSettingsData(d, d.Current(), d.HowIWork())
	pd.Onboarding = r.URL.Query().Get("from") == "onboarding"
	v := view{User: id, Nav: "settings", D: pd}
	if r.URL.Query().Get("saved") == "1" {
		v.Notice = "Your settings are saved. The helper reads them from the next new chat."
	}
	s.render(w, r, http.StatusOK, "settings.html", v)
}

func formAnswers(r *http.Request) settings.Answers {
	a := settings.Answers{
		AddressForm: settings.AddressForm(r.FormValue("address_form")), Language: settings.Language(r.FormValue("lang")),
		Tone: settings.Tone(r.FormValue("tone")), Length: settings.Length(r.FormValue("length")),
		Field: r.FormValue("field"), Always: r.FormValue("always"), Never: r.FormValue("never"), Note: r.FormValue("note"),
	}
	for _, f := range r.Form["formats"] {
		a.Formats = append(a.Formats, settings.Format(f))
	}
	for _, t := range r.Form["tasks"] {
		a.Tasks = append(a.Tasks, settings.Task(t))
	}
	a.Clean()
	return a
}

// settingsSubmit handles both buttons of the form: "aktion=vorschlag" fills
// the text from the answers (nothing stored), anything else saves.
func (s *Server) settingsSubmit(w http.ResponseWriter, r *http.Request, id Identity) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Invalid request", "The form was too large or incomplete.")
		return
	}
	d, err := s.settings.Load(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	a := formAnswers(r)
	onboarding := r.FormValue("from") == "onboarding"
	var errs []fieldError
	for _, e := range a.Validate() {
		errs = append(errs, fieldError{e.Field, e.Msg})
	}
	text, terr := settings.CleanText(r.FormValue("text"))
	if terr != nil {
		errs = append(errs, fieldError{"text", terr.Error()})
		text = r.FormValue("text")
	}
	pd := newSettingsData(d, a, text)
	pd.Onboarding = onboarding
	if len(errs) > 0 {
		pd.Block = ""
		s.render(w, r, http.StatusUnprocessableEntity, "settings.html", view{User: id, Nav: "settings", Errors: errs, D: pd})
		return
	}
	if r.FormValue("action") == "suggest" {
		pd.Text, pd.LLM = s.settings.Suggest(r.Context(), id, a)
		pd.Suggestion, pd.Block = true, settings.Block(d.Facts, a, pd.Text, d.Lang)
		s.render(w, r, http.StatusOK, "settings.html", view{User: id, Nav: "settings", D: pd,
			Notice: "Here is a suggestion. Check and edit the text, then save it."})
		return
	}
	if err := s.settings.Save(r.Context(), id, a, text); errors.Is(err, settings.ErrTooLarge) {
		errs = []fieldError{{"text", "The settings text is too long for the helper’s memory (at most 8 KB together with what it has remembered). Please shorten the text."}}
		s.render(w, r, http.StatusUnprocessableEntity, "settings.html", view{User: id, Nav: "settings", Errors: errs, D: pd})
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	if onboarding {
		http.Redirect(w, r, "/onboarding?step="+strconv.Itoa(s.onboardingStepCount()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// onboardingStepCount: the Settings step sits between Connections and the first task.
func (s *Server) onboardingStepCount() int {
	if s.settings != nil {
		return onboardingSteps + 1
	}
	return onboardingSteps
}

func (p settingsData) HasFormat(f settings.Format) bool { return slices.Contains(p.A.Formats, f) }
func (p settingsData) HasTask(t settings.Task) bool     { return slices.Contains(p.A.Tasks, t) }
