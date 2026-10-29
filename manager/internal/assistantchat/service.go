// SPDX-License-Identifier: MIT

// Package assistantchat runs restricted support conversations outside the shared
// runtime. It has no filesystem, shell, delegation or arbitrary HTTP executor.
package assistantchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

var ErrDenied = errors.New("assistant access unavailable")
var ErrBusy = errors.New("a reply is already running; reload after it finishes")
var ErrMessageSize = errors.New("message must contain 1 to 16000 bytes")
var ErrFull = errors.New("conversation is full; start a new chat")

const TurnTimeout = 2 * time.Minute

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Service struct {
	Store    *store.Store
	Items    func(assistants.Definition) []catalog.Item
	Tools    func(context.Context, store.User, func() (assistants.Pin, error)) ([]*sdk.Tool, error)
	Call     func(context.Context, store.User, string, string, json.RawMessage, func() (assistants.Pin, error)) (*sdk.CallToolResult, error)
	Complete func(context.Context, store.User, string, string, int, any, any) (json.RawMessage, error)
}

type Choice struct {
	assistants.Definition
	Missing string
	Sources []string
}

func (s *Service) Choices(ctx context.Context, uid string, groups []string) []Choice {
	var out []Choice
	for _, p := range instance.Current().Assistants {
		if !p.Available(groups) {
			continue
		}
		c := Choice{Definition: p}
		sources, _ := p.Sources(instance.Current().KnowledgeSources)
		for _, source := range sources {
			c.Sources = append(c.Sources, source.Instruction)
		}
		if err := s.prerequisites(p); err != nil {
			c.Missing = err.Error()
		}
		out = append(out, c)
	}
	return out
}

func (s *Service) prerequisites(p assistants.Definition) error {
	if s.Tools == nil || s.Call == nil || s.Complete == nil || s.Items == nil {
		return errors.New("assistant service unavailable")
	}
	sources, err := p.Sources(instance.Current().KnowledgeSources)
	if err != nil {
		return err
	}
	items := s.Items(p)
	for _, source := range sources {
		entry, server := source.Server()
		if !slices.ContainsFunc(items, func(it catalog.Item) bool {
			return it.ID == entry && instance.Current().Enabled(entry) && slices.Contains(it.LiteLLM, server) && (len(source.Tools) == 0 || slices.ContainsFunc(source.Tools, func(t string) bool { return slices.Contains(it.Tools, t) }))
		}) {
			return fmt.Errorf("required knowledge source unavailable: %s", entry)
		}
	}
	for _, logical := range p.MCPTools {
		server, tool, _ := strings.Cut(logical, ".")
		if server != instance.Current().MCPServer || !instance.Current().ToolEnabled(tool) || !slices.ContainsFunc(items, func(it catalog.Item) bool {
			return it.Kind == catalog.KindMCP && slices.Contains(it.Tools, tool)
		}) {
			return fmt.Errorf("required organisation tool unavailable: %s", logical)
		}
	}
	return nil
}

func (s *Service) definition(ctx context.Context, uid string, groups []string, id string) (assistants.Definition, error) {
	p, ok := instance.Current().Assistant(id)
	if !ok || !p.Available(groups) {
		return p, ErrDenied
	}
	if ok, err := s.Store.HasConsent(ctx, uid, consent.Version()); err != nil || !ok {
		return p, ErrDenied
	}
	if _, err := s.Store.AssistantAccount(ctx, uid); err != nil {
		return p, ErrDenied
	}
	return p, s.prerequisites(p)
}

// Create accepts only a assistant ID from the form. Pins and project IDs are
// resolved here, after the portal has authenticated identity and checked CSRF.
func (s *Service) Create(ctx context.Context, uid string, groups []string, id, hash, description string, evaluate bool) (string, error) {
	p, err := s.definition(ctx, uid, groups, id)
	if err != nil {
		return "", err
	}
	pin := instance.Current().AssistantPin(p)
	if hash != pin.Hash || (evaluate && !p.Evaluation) {
		return "", ErrDenied
	}
	description = strings.TrimSpace(description)
	if len(description) < 10 || len(description) > 4000 {
		return "", errors.New("describe the task in 10 to 4000 bytes")
	}
	idv := uuid.NewString()
	name := p.ID + "-" + idv[:8]
	project := store.Project{ID: idv, UID: uid, Folder: name, Description: description, Assistant: pin, CreatedAt: time.Now()}
	if err := s.Store.CreateAssistantProject(ctx, project, evaluate); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Service) Open(ctx context.Context, uid string, groups []string, folder string) (string, error) {
	p, err := s.Store.ProjectByFolder(ctx, uid, folder)
	if err != nil || !p.Isolated {
		return "", ErrDenied
	}
	def, err := s.definition(ctx, uid, groups, p.Assistant.ID)
	if err != nil {
		return "", err
	}
	if instance.Current().AssistantPin(def) != p.Assistant {
		return "", ErrDenied
	}
	id := "ses_profile_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := s.Store.CreateAssistantSession(ctx, uid, p.ID, id, time.Now()); err != nil {
		return "", err
	}
	_ = s.Store.TouchProject(ctx, uid, folder, time.Now())
	return id, nil
}

// View is owner-only. Reading/exporting one's old conversation does not require
// current tool grants; sending another turn always revalidates them.
func (s *Service) View(ctx context.Context, uid, id string) (store.Project, store.AssistantSession, []Message, error) {
	se, err := s.Store.AssistantSession(ctx, uid, id)
	if err != nil {
		return store.Project{}, se, nil, ErrDenied
	}
	p, err := s.Store.AssistantProjectByID(ctx, uid, se.ProjectID)
	if err != nil {
		return p, se, nil, ErrDenied
	}
	var messages []Message
	err = json.Unmarshal(se.Messages, &messages)
	return p, se, messages, err
}

// Authorize checks audience, account, consent and the assistant-owned source grants.
func (s *Service) Authorize(ctx context.Context, uid string, groups []string, id, hash string) (assistants.Definition, error) {
	p, err := s.definition(ctx, uid, groups, id)
	if err == nil && instance.Current().AssistantPin(p).Hash != hash {
		err = ErrDenied
	}
	return p, err
}

func (s *Service) Send(ctx context.Context, uid string, groups []string, id, text string, revision int) error {
	if strings.TrimSpace(text) == "" || len(text) > 16000 {
		return ErrMessageSize
	}
	ctx, cancel := context.WithTimeout(ctx, TurnTimeout)
	defer cancel()
	until, _ := ctx.Deadline()
	p, se, history, err := s.View(ctx, uid, id)
	if err != nil {
		return err
	}
	if revision != se.Revision {
		return ErrBusy
	}
	if len(se.Messages) > 512<<10 {
		return ErrFull
	}
	account, err := s.Store.AssistantAccount(ctx, uid)
	if err != nil {
		return ErrDenied
	}
	authorize := func() (assistants.Pin, error) {
		if ctx.Err() != nil {
			return assistants.Pin{}, ctx.Err()
		}
		fresh, err := s.Store.AssistantProjectByID(ctx, uid, p.ID)
		if err != nil || fresh.Assistant != p.Assistant {
			return assistants.Pin{}, ErrDenied
		}
		stamp, err := s.Store.AssistantAccount(ctx, uid)
		if err != nil || stamp != account {
			return assistants.Pin{}, ErrDenied
		}
		def, err := s.definition(ctx, uid, groups, p.Assistant.ID)
		if err != nil {
			return assistants.Pin{}, err
		}
		if instance.Current().AssistantPin(def) != p.Assistant {
			return assistants.Pin{}, ErrDenied
		}
		return p.Assistant, nil
	}
	if _, err := authorize(); err != nil {
		return err
	}
	if err := s.Store.ClaimAssistantTurn(ctx, se, until); err != nil {
		return ErrBusy
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.Store.ReleaseAssistantTurn(c, se, until)
	}()
	u, err := s.Store.UserByUID(ctx, uid)
	if err != nil {
		return err
	}
	def, _ := instance.Current().Assistant(p.Assistant.ID)
	sources, _ := def.Sources(instance.Current().KnowledgeSources)
	prompt := string(def.RenderAgent(sources...))
	// The renderer's preview frontmatter is metadata, not a runtime configuration.
	if _, rest, ok := strings.Cut(strings.TrimPrefix(prompt, "---\n"), "---\n"); ok {
		prompt = rest
	}
	messages := append([]Message{{Role: "system", Content: prompt}}, history...)
	messages = append(messages, Message{Role: "user", Content: text})
	native, err := s.Tools(ctx, u, authorize)
	if err != nil {
		return err
	}
	var tools []map[string]any
	allowed := map[string]bool{}
	for _, t := range native {
		allowed[t.Name] = true
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}})
	}
	model := instance.Current().Models.Default
	switch def.ModelRole {
	case "small":
		model = instance.Current().Models.Small
	case "thorough":
		model = instance.Current().Models.Thorough
	}
	for step := 0; step < 8; step++ {
		if _, err := authorize(); err != nil {
			return err
		}
		var offered any
		if len(tools) > 0 {
			offered = tools
		}
		raw, err := s.Complete(ctx, u, id, model, 4096, messages, offered)
		if err != nil {
			return err
		}
		if _, err := authorize(); err != nil {
			return err
		}
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil || m.Role != "assistant" || len(m.ToolCalls) > 8 {
			return errors.New("invalid model response")
		}
		messages = append(messages, m)
		if len(m.ToolCalls) == 0 {
			if strings.TrimSpace(m.Content) == "" {
				return errors.New("empty answer")
			}
			b, err := json.Marshal(messages[1:])
			if err != nil {
				return err
			}
			if len(b) > 1<<20 {
				return ErrFull
			}
			if _, err = authorize(); err != nil {
				return err
			}
			if err = s.Store.CommitAssistantTurn(ctx, se, p.Assistant, account, consent.Version(), until, b); err != nil {
				return err
			}
			_ = s.Store.TouchProject(ctx, uid, p.Folder, time.Now())
			return s.collect(ctx, p, se, messages[1:])
		}
		for _, call := range m.ToolCalls {
			if call.Type != "function" || call.ID == "" || !allowed[call.Function.Name] || !json.Valid([]byte(call.Function.Arguments)) {
				return errors.New("capability outside assistant")
			}
			result, err := s.Call(ctx, u, id, call.Function.Name, json.RawMessage(call.Function.Arguments), authorize)
			if err != nil {
				return err
			}
			if _, err := authorize(); err != nil {
				return err
			}
			b, err := json.Marshal(result)
			if err != nil {
				return err
			}
			if len(b) > 128<<10 {
				return errors.New("source response too large")
			}
			messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Content: string(b)})
		}
	}
	return errors.New("source lookup limit reached; narrow the question or contact support")
}

func (s *Service) collect(ctx context.Context, p store.Project, se store.AssistantSession, messages []Message) error {
	def, ok := instance.Current().Assistant(p.Assistant.ID)
	if !ok || !def.Evaluation || !instance.Current().Enabled("evaluation") {
		return nil
	}
	on, err := s.Store.AssistantEvaluationOptIn(ctx, p.UID, p.ID)
	if err != nil || !on {
		return err
	}
	var b strings.Builder
	for _, m := range messages {
		if m.Role == "user" || m.Role == "assistant" {
			fmt.Fprintf(&b, "%s: %s\n\n", m.Role, m.Content)
		}
	}
	text := b.String()
	truncated := len(text) > 64<<10
	if truncated {
		text = text[:64<<10]
	}
	return s.Store.PutAssistantChat(ctx, store.Chat{UID: p.UID, Project: p.Folder, SessionID: se.ID, Title: p.Description, Created: se.Updated, Updated: time.Now(), CollectedAt: time.Now(), Messages: len(messages), Transcript: text, Truncated: truncated})
}
