// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"fmt"
	"slices"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// ValidateSources checks references against existing transports, without probing
// upstream services. Tool annotations are checked on each list and call.
func ValidateSources(registry assistants.Sources, items []catalog.Item) error {
	if err := registry.Validate(); err != nil {
		return err
	}
	for id, source := range registry {
		entry, server := source.Server()
		i := slices.IndexFunc(items, func(it catalog.Item) bool {
			return it.ID == entry && it.Kind == catalog.KindCentral && slices.Contains(it.LiteLLM, server)
		})
		if i < 0 {
			return fmt.Errorf("knowledge source %s: unknown MCP server %s", id, source.MCPServer)
		}
		for _, tool := range source.Tools {
			if !slices.Contains(items[i].Tools, tool) {
				return fmt.Errorf("knowledge source %s: tool %s is not granted by catalog", id, tool)
			}
		}
	}
	return nil
}

func (s *Server) sourceScope(ctx context.Context, uid string, it *catalog.Item) ([]assistants.Source, error) {
	if s.Assistant == nil {
		return nil, nil
	}
	pin, err := s.Assistant(ctx, uid)
	if err != nil {
		return nil, err
	}
	if pin == (assistants.Pin{}) {
		return nil, nil
	}
	return scopeForPin(pin, it)
}

func scopeForPin(pin assistants.Pin, it *catalog.Item) ([]assistants.Source, error) {
	d := instance.Current()
	p, ok := d.Assistant(pin.ID)
	if !ok || d.AssistantPin(p) != pin {
		return nil, fmt.Errorf("assistant pin is stale or unknown")
	}
	sources, err := p.Sources(d.KnowledgeSources)
	if err != nil {
		return nil, err
	}
	scope := []assistants.Source{}
	servers := []string{}
	for _, source := range sources {
		entry, server := source.Server()
		if entry == it.ID && slices.Contains(it.LiteLLM, server) {
			scope = append(scope, source)
			servers = append(servers, server)
		}
	}
	if len(scope) == 0 {
		return nil, fmt.Errorf("MCP server is outside assistant sources")
	}
	it.LiteLLM = servers
	return scope, nil
}
