// SPDX-License-Identifier: MIT

package portal

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistantchat"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func init() { pageFiles = append(pageFiles, "assistants.html", "support-chat.html") }

func (s *Server) taskAssistantRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.taskAssistants == nil {
		return
	}
	page("GET /assistants", s.taskAssistantForm)
	page("GET /new", s.taskAssistantForm)
	page("POST /new", s.taskAssistantCreate)
	page("GET /support/{session}", s.supportChat)
	page("POST /support/{session}", s.supportSend)
	page("POST /support/{session}/evaluation/withdraw", s.supportWithdraw)
}

type assistantCard struct {
	assistantchat.Choice
	Hash    string
	Focused bool
	OnStart bool
}

type taskAssistantData struct{ Cards []assistantCard }

func (s *Server) assistantCards(r *http.Request, id Identity, wanted string, onStart bool) []assistantCard {
	if s.taskAssistants == nil {
		return nil
	}
	var cards []assistantCard
	for _, choice := range s.taskAssistants.Choices(r.Context(), id.UID, id.Groups) {
		// Hash the configured definition, before applying the localized default.
		hash := instance.Current().AssistantPin(choice.Definition).Hash
		choice.Missing = assistantErrorText(lang(r), choice.Missing)
		if lang(r) == langEN {
			choice.Title = cmp.Or(choice.TitleEN, choice.Title)
			choice.Description = cmp.Or(choice.DescriptionEN, choice.Description)
			choice.EntryQuestion = cmp.Or(choice.EntryQuestionEN, choice.EntryQuestion)
		}
		if strings.TrimSpace(choice.EntryQuestion) == "" {
			choice.EntryQuestion = tr(lang(r), "How can I help you?")
		}
		cards = append(cards, assistantCard{Choice: choice, Hash: hash, Focused: choice.ID == wanted, OnStart: onStart})
	}
	return cards
}

func (s *Server) taskAssistantForm(w http.ResponseWriter, r *http.Request, id Identity) {
	wanted := cmp.Or(r.URL.Query().Get("assistant"), r.URL.Query().Get("profile"))
	d := taskAssistantData{Cards: s.assistantCards(r, id, wanted, false)}
	if wanted != "" && !slices.ContainsFunc(d.Cards, func(c assistantCard) bool { return c.Focused }) {
		s.assistantError(w, r, id, assistantchat.ErrDenied)
		return
	}
	s.render(w, r, http.StatusOK, "assistants.html", view{User: id, Nav: "assistants", D: d})
}

func (s *Server) taskAssistantCreate(w http.ResponseWriter, r *http.Request, id Identity) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		s.assistantError(w, r, id, assistantchat.ErrDenied)
		return
	}
	question := r.PostForm.Get("question")
	if strings.TrimSpace(question) == "" || len(question) > 16000 || strings.ContainsRune(question, '\x00') {
		s.errorPage(w, r, http.StatusUnprocessableEntity, "New project", "message must contain 1 to 16000 bytes")
		return
	}
	p, err := s.taskAssistants.Authorize(r.Context(), id.UID, id.Groups, cmp.Or(r.PostForm.Get("assistant"), r.PostForm.Get("profile")), r.PostForm.Get("hash"))
	if err != nil {
		s.assistantError(w, r, id, err)
		return
	}
	name, prompt, err := s.be.PrepareAssistant(r.Context(), id.UID, p.InitPrompt, question, p.ProjectMode)
	if p.ProjectMode == "shared" {
		name = p.SharedFolder()
		if err != nil {
			prompt = assistantPrompt(p.InitPrompt, question)
		}
	} else {
		name = normalizeName(name)
		if err != nil || validName(name) != "" {
			name, prompt = fallbackName(question, s.now()), assistantPrompt(p.InitPrompt, question)
		}
		name, _, err = s.uniqueName(r.Context(), id.UID, name, false)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	// Revalidate after the model call, before creating persistent state.
	if _, err = s.taskAssistants.Authorize(r.Context(), id.UID, id.Groups, p.ID, r.PostForm.Get("hash")); err != nil {
		s.assistantError(w, r, id, err)
		return
	}
	if err = s.be.CreateAssistantProject(r.Context(), id.UID, id.Groups, NewProject{Name: name, Description: question}, p.ID, r.PostForm.Get("hash")); err != nil {
		s.serverError(w, r, err)
		return
	}
	link, err := s.be.CreateSession(r.Context(), id.UID, name, prompt)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	navigate(w, r, link)
}

func assistantPrompt(context, question string) string { return context + "\n\n" + question }

type supportData struct {
	Project    store.Project
	Session    store.AssistantSession
	Messages   []assistantchat.Message
	Prompt     string
	Evaluation bool
}

func (s *Server) supportChat(w http.ResponseWriter, r *http.Request, id Identity) {
	p, se, messages, err := s.taskAssistants.View(r.Context(), id.UID, r.PathValue("session"))
	if err != nil {
		s.assistantError(w, r, id, err)
		return
	}
	d := supportData{Project: p, Session: se, Messages: messages}
	if se.Revision == 0 {
		d.Prompt = p.Description
	}
	d.Evaluation, _ = s.taskAssistants.Store.AssistantEvaluationOptIn(r.Context(), id.UID, p.ID)
	s.render(w, r, http.StatusOK, "support-chat.html", view{User: id, Nav: "projects", D: d})
}

func (s *Server) supportSend(w http.ResponseWriter, r *http.Request, id Identity) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	revision, err := strconv.Atoi(r.PostForm.Get("revision"))
	if err != nil {
		s.assistantError(w, r, id, assistantchat.ErrDenied)
		return
	}
	if err := s.taskAssistants.Send(r.Context(), id.UID, id.Groups, r.PathValue("session"), r.PostForm.Get("message"), revision); err != nil {
		switch {
		case errors.Is(err, assistantchat.ErrDenied):
			s.assistantError(w, r, id, assistantchat.ErrDenied)
		case errors.Is(err, assistantchat.ErrBusy):
			s.assistantError(w, r, id, assistantchat.ErrBusy)
		case errors.Is(err, assistantchat.ErrMessageSize), errors.Is(err, assistantchat.ErrFull):
			s.assistantError(w, r, id, err)
		default:
			s.serverError(w, r, err)
		}
		return
	}
	navigate(w, r, "/support/"+r.PathValue("session"))
}
func (s *Server) supportWithdraw(w http.ResponseWriter, r *http.Request, id Identity) {
	p, _, _, err := s.taskAssistants.View(r.Context(), id.UID, r.PathValue("session"))
	if err != nil {
		s.assistantError(w, r, id, err)
		return
	}
	if err := s.taskAssistants.Store.WithdrawAssistantEvaluation(r.Context(), id.UID, p.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	navigate(w, r, "/support/"+r.PathValue("session"))
}
func (s *Server) assistantError(w http.ResponseWriter, r *http.Request, id Identity, err error) {
	s.render(w, r, http.StatusForbidden, "error.html", view{User: id, D: errorData{Title: "Assistant unavailable", Text: assistantErrorText(lang(r), err.Error())}})
}

// Render text and HTTP source links without accepting model-written HTML.
type supportPart struct{ Text, URL string }

var supportURL = regexp.MustCompile(`https?://[^\s<>"\[\]]+`)

func supportParts(text string) []supportPart {
	var out []supportPart
	last := 0
	for _, loc := range supportURL.FindAllStringIndex(text, -1) {
		raw := strings.TrimRight(text[loc[0]:loc[1]], ".,;:!?)")
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil {
			continue
		}
		out = append(out, supportPart{Text: text[last:loc[0]]}, supportPart{Text: raw, URL: raw})
		last = loc[0] + len(raw)
	}
	return append(out, supportPart{Text: text[last:]})
}

func assistantErrorText(l, text string) string {
	if source, ok := strings.CutPrefix(text, "required knowledge source unavailable: "); ok {
		return trf(l, "Required knowledge source unavailable: %s.", source)
	}
	if tool, ok := strings.CutPrefix(text, "required organisation tool unavailable: "); ok {
		return trf(l, "Required organisation tool unavailable: %s.", tool)
	}
	return tr(l, text)
}
