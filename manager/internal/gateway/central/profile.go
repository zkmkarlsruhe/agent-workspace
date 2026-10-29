// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// AssistantTools and AssistantCall require manager-validated authorization from
// private chat state or the stored project and runtime session binding.
func (s *Server) AssistantTools(ctx context.Context, u store.User, authorize func() (assistants.Pin, error)) ([]*sdk.Tool, error) {
	pin, err := authorize()
	if err != nil {
		return nil, err
	}
	p, ok := instance.Current().Assistant(pin.ID)
	if !ok {
		return nil, errors.New("assistant unavailable")
	}
	sources, err := p.Sources(instance.Current().KnowledgeSources)
	if err != nil {
		return nil, err
	}
	var out []*sdk.Tool
	seen := map[string]bool{}
	for _, source := range sources {
		entry, _ := source.Server()
		if seen[entry] {
			continue
		}
		seen[entry] = true
		it := catalog.Item{}
		if _, err := authorize(); err != nil {
			return nil, err
		}
		for _, candidate := range s.assistantItems(pin) {
			if candidate.ID == entry && instance.Current().RemoteEnabled(entry) {
				it = candidate
			}
		}
		if it.ID == "" {
			return nil, errors.New("required knowledge source unavailable: " + entry)
		}
		scope, err := scopeForPin(pin, &it)
		if err != nil {
			return nil, err
		}
		result, err := s.listTools(ctx, u, it, scope)
		if err != nil {
			return nil, err
		}
		if len(result.Tools) == 0 {
			return nil, errors.New("required knowledge source has no permitted tools: " + entry)
		}
		for _, tool := range result.Tools {
			tool.Name = entry + "_" + tool.Name
			out = append(out, tool)
		}
	}
	_, err = authorize()
	return out, err
}

func (s *Server) AssistantCall(ctx context.Context, u store.User, session, name string, args json.RawMessage, authorize func() (assistants.Pin, error)) (*sdk.CallToolResult, error) {
	entry, tool, ok := strings.Cut(name, "_")
	if !ok {
		return nil, errors.New("capability outside assistant")
	}
	check := func() (assistants.Pin, error) {
		pin, err := authorize()
		if err != nil {
			return pin, err
		}
		for _, it := range s.assistantItems(pin) {
			if it.ID == entry && instance.Current().RemoteEnabled(entry) && slices.Contains(it.Tools, tool) {
				_, err := scopeForPin(pin, &it)
				return pin, err
			}
		}
		return assistants.Pin{}, errors.New("assistant capability grant revoked")
	}
	ctx = context.WithValue(ctx, assistantAuthorizationKey{}, check)
	pin, err := check()
	if err != nil {
		return nil, err
	}
	for _, it := range s.assistantItems(pin) {
		if it.ID != entry || !instance.Current().RemoteEnabled(entry) {
			continue
		}
		scope, err := scopeForPin(pin, &it)
		if err != nil {
			return nil, err
		}
		result := s.callTool(ctx, u, it, scope, &sdk.CallToolParamsRaw{Name: tool, Arguments: args, Meta: sdk.Meta{"ai.opencode/sessionID": session}})
		if _, err := check(); err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, errors.New("required knowledge source unavailable: " + entry)
}

type assistantAuthorizationKey struct{}

// Only a validated pin selects assistant grants; personal catalog state is separate.
func (s *Server) assistantItems(pin assistants.Pin) []catalog.Item {
	p, ok := instance.Current().Assistant(pin.ID)
	if !ok || !p.Enabled || instance.Current().AssistantPin(p) != pin || s.AssistantItems == nil {
		return nil
	}
	return s.AssistantItems(p)
}
