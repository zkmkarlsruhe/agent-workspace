// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
	"go.yaml.in/yaml/v3"
)

func init() {
	pageFiles = append(pageFiles, "admin-assistants.html", "admin-assistant.html", "assistant-test.html")
}

func (s *Server) assistantAdminRoutes(a func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.taskAssistants == nil || s.taskAssistants.Store == nil {
		return
	}
	a("GET /admin/assistants", s.adminAssistants)
	a("GET /admin/assistants/{assistant}", s.adminAssistant)
	a("POST /admin/assistants/{assistant}", s.saveAssistant)
	a("POST /admin/assistants/{assistant}/export", s.exportAssistant)
	a("POST /admin/assistants/{assistant}/test", s.testAssistant)
	s.gapAdminRoutes(a)
}

type assistantField struct {
	Key, Label, Kind, Value string
	Options                 []string
	Selected                []string
}

// One field inventory covers rendering and parsing; unlisted input cannot alter a definition.
func assistantFields(p assistants.Definition) []assistantField {
	fields := []assistantField{
		{Key: "owner", Label: "Content owner"}, {Key: "title", Label: "Title"}, {Key: "description", Label: "Description", Kind: "text"},
		{Key: "title_en", Label: "English title"}, {Key: "description_en", Label: "English description", Kind: "text"},
		{Key: "entry_question", Label: "Entry question"}, {Key: "entry_question_en", Label: "English entry question"},
		{Key: "init_prompt", Label: "Initial context", Kind: "text"},
		{Key: "role", Label: "Role", Kind: "text"}, {Key: "rules", Label: "Rules (one per line)", Kind: "lines"}, {Key: "escalation", Label: "Escalation", Kind: "text"},
		{Key: "audience", Label: "Audience groups (one per line; empty means all accounts)", Kind: "lines"},
		{Key: "knowledge_sources", Label: "Knowledge sources", Kind: "multi"},
		{Key: "skills", Label: "Skills (one per line)", Kind: "lines"},
		{Key: "mcp_tools", Label: "Organisation tools", Kind: "multi"},
		{Key: "builtin_tools", Label: "Built-in tools", Kind: "multi", Options: []string{"question", "read", "glob", "grep", "shell", "edit", "write", "patch"}},
		{Key: "model_role", Label: "Model role", Kind: "select", Options: []string{"default", "small", "thorough"}},
		{Key: "project_mode", Label: "Project mode", Kind: "select", Options: []string{"per_task", "shared"}},
		{Key: "temperature", Label: "Temperature", Kind: "float"}, {Key: "top_p", Label: "Top P", Kind: "float"},
		{Key: "max_output_tokens", Label: "Maximum output tokens", Kind: "integer"}, {Key: "steps", Label: "Maximum steps", Kind: "integer"},
		{Key: "reasoning_effort", Label: "Reasoning effort", Kind: "select", Options: []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max"}},
		{Key: "icon", Label: "Icon", Kind: "select", Options: []string{"", "chat", "search", "book", "tools"}},
		{Key: "color", Label: "Colour"}, {Key: "consent_mode", Label: "Consent mode", Kind: "select", Options: []string{"pending", "support"}},
		{Key: "enabled", Label: "Enabled", Kind: "bool"}, {Key: "hidden", Label: "Hidden", Kind: "bool"},
		{Key: "evaluation", Label: "Allow separate evaluation opt-in", Kind: "bool"},
		{Key: "gap_collection", Label: "Offer voluntary knowledge gap submission", Kind: "bool"},
	}
	raw, _ := json.Marshal(p)
	var values map[string]any
	_ = json.Unmarshal(raw, &values)
	prompt := values["prompt"].(map[string]any)
	for k, v := range prompt {
		values[k] = v
	}
	for i := range fields {
		f := &fields[i]
		if f.Key == "knowledge_sources" {
			for id := range instance.Current().KnowledgeSources {
				f.Options = append(f.Options, id)
			}
			slices.Sort(f.Options)
		}
		if f.Key == "mcp_tools" {
			for _, name := range []string{"web_search", "web_read"} {
				if instance.Current().ToolEnabled(name) {
					f.Options = append(f.Options, instance.Current().MCPServer+"."+name)
				}
			}
		}
		switch v := values[f.Key].(type) {
		case []any:
			for _, item := range v {
				f.Selected = append(f.Selected, item.(string))
			}
			f.Value = strings.Join(f.Selected, "\n")
		case string:
			f.Value = v
		case bool:
			if v {
				f.Value = "true"
			}
		case float64:
			f.Value = strconv.FormatFloat(v, 'f', -1, 64)
		}
		if f.Key == "project_mode" && f.Value == "" {
			f.Value = "per_task"
		}
	}
	return fields
}

func parseAssistant(r *http.Request, p assistants.Definition) (assistants.Definition, error) {
	raw, _ := json.Marshal(p)
	var values map[string]any
	_ = json.Unmarshal(raw, &values)
	prompt := values["prompt"].(map[string]any)
	for _, f := range assistantFields(p) {
		text := strings.TrimSpace(r.PostForm.Get(f.Key))
		var value any = text
		switch f.Kind {
		case "bool":
			value = r.PostForm.Get(f.Key) == "true"
		case "integer":
			if text == "" {
				value = nil
			} else {
				n, err := strconv.Atoi(text)
				if err != nil {
					return p, fmt.Errorf("%s: invalid integer", f.Label)
				}
				value = n
			}
		case "float":
			if text == "" {
				value = nil
			} else {
				n, err := strconv.ParseFloat(text, 64)
				if err != nil {
					return p, fmt.Errorf("%s: invalid number", f.Label)
				}
				value = n
			}
		case "lines":
			list := []string{}
			for _, line := range strings.Split(text, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					list = append(list, line)
				}
			}
			value = list
		case "multi":
			list := r.PostForm[f.Key]
			for _, item := range list {
				if !slices.Contains(f.Options, item) {
					return p, fmt.Errorf("%s: unapproved reference", f.Label)
				}
			}
			value = list
		case "select":
			if !slices.Contains(f.Options, text) {
				return p, fmt.Errorf("%s: invalid selection", f.Label)
			}
		}
		if _, ok := prompt[f.Key]; ok {
			prompt[f.Key] = value
		} else {
			values[f.Key] = value
		}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(raw, &p)
	return p, err
}

type assistantEditorData struct {
	Definition       assistants.Definition
	Fields           []assistantField
	Hash, Error      string
	Projects, People int
	New              bool
}

func (s *Server) editorData(ctx context.Context, name string) (assistantEditorData, error) {
	d := assistantEditorData{New: name == "new"}
	if d.New {
		d.Definition = assistants.Definition{Version: 1, ModelRole: "default", ProjectMode: "per_task", ConsentMode: "pending", Prompt: assistants.Prompt{Rules: []string{}}}
	} else {
		current := instance.Current()
		definitions, err := s.taskAssistants.Store.StoredAssistants(ctx)
		if err != nil {
			return d, err
		}
		found := false
		for _, p := range definitions {
			if p.ID == name {
				d.Definition, d.Error, found = p.Definition, p.ValidationError, true
				break
			}
		}
		if !found {
			return d, store.ErrNotFound
		}
		d.Hash = current.AssistantPin(d.Definition).Hash
	}
	d.Fields = assistantFields(d.Definition)
	var err error
	if !d.New {
		d.Projects, d.People, err = s.taskAssistants.Store.AssistantProjectCount(ctx, name)
	}
	return d, err
}

func (s *Server) adminAssistants(w http.ResponseWriter, r *http.Request, id Identity) {
	var rows []assistantEditorData
	definitions, err := s.taskAssistants.Store.StoredAssistants(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, p := range definitions {
		d, err := s.editorData(r.Context(), p.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		rows = append(rows, d)
	}
	s.render(w, r, 200, "admin-assistants.html", view{User: id, Nav: "assistant-admin", D: rows})
}

func (s *Server) adminAssistant(w http.ResponseWriter, r *http.Request, id Identity) {
	d, err := s.editorData(r.Context(), r.PathValue("assistant"))
	if err != nil {
		s.editorError(w, r, err)
		return
	}
	s.render(w, r, 200, "admin-assistant.html", view{User: id, Nav: "assistant-admin", D: d})
}

func (s *Server) editorError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusUnprocessableEntity
	if errors.Is(err, store.ErrNotFound) {
		status = 404
	}
	if errors.Is(err, store.ErrAssistantConflict) {
		status = 409
	}
	s.errorPage(w, r, status, "Request refused", err.Error())
}
func editorForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	return r.ParseForm()
}

func (s *Server) saveAssistant(w http.ResponseWriter, r *http.Request, id Identity) {
	if err := editorForm(w, r); err != nil {
		s.editorError(w, r, err)
		return
	}
	d, err := s.editorData(r.Context(), r.PathValue("assistant"))
	if err != nil {
		s.editorError(w, r, err)
		return
	}
	if r.PostForm.Get("hash") != d.Hash {
		s.editorError(w, r, store.ErrAssistantConflict)
		return
	}
	p, err := parseAssistant(r, d.Definition)
	if d.New {
		p.ID = strings.TrimSpace(r.PostForm.Get("id"))
		if p.ID == "new" {
			err = errors.New("reserved assistant ID")
		}
	}
	if err == nil {
		err = s.taskAssistants.ValidateDefinition(p)
	}
	if err == nil {
		_, err = s.taskAssistants.Store.SaveAssistant(r.Context(), p, d.Hash, id.UID)
	}
	if err != nil {
		if errors.Is(err, store.ErrAssistantConflict) {
			s.editorError(w, r, err)
			return
		}
		d.Definition = p
		d.Fields = assistantFields(p)
		d.Error = err.Error()
		s.render(w, r, 422, "admin-assistant.html", view{User: id, Nav: "assistant-admin", D: d})
		return
	}
	http.Redirect(w, r, "/admin/assistants/"+p.ID+"?saved=1", http.StatusSeeOther)
}

func (s *Server) savedAssistant(w http.ResponseWriter, r *http.Request) (assistants.Definition, error) {
	if err := editorForm(w, r); err != nil {
		return assistants.Definition{}, err
	}
	d := instance.Current()
	p, ok := d.Assistant(r.PathValue("assistant"))
	if !ok {
		return p, store.ErrNotFound
	}
	if r.PostForm.Get("hash") != d.AssistantPin(p).Hash {
		return p, store.ErrAssistantConflict
	}
	return p, nil
}

func (s *Server) exportAssistant(w http.ResponseWriter, r *http.Request, id Identity) {
	p, err := s.savedAssistant(w, r)
	if err != nil {
		s.editorError(w, r, err)
		return
	}
	if err := s.taskAssistants.Store.AuditAssistant(r.Context(), id.UID, p.ID, p.Version, instance.Current().AssistantPin(p).Hash, "assistant_export"); err != nil {
		s.serverError(w, r, err)
		return
	}
	body, err := yaml.Marshal(struct {
		Assistants []assistants.Definition `yaml:"assistants"`
	}{[]assistants.Definition{p}})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="assistant-`+p.ID+`.yaml"`)
	_, _ = w.Write(body)
}
