// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// AssistantDraftCompletion uses the same model body and accounting as the rendered assistant.
func (g *Gateway) AssistantDraftCompletion(ctx context.Context, u store.User, session string, p assistants.Definition, messages any) (json.RawMessage, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	model := instance.Current().Models.Default
	if p.ModelRole == "small" {
		model = instance.Current().Models.Small
	}
	if p.ModelRole == "thorough" {
		model = instance.Current().Models.Thorough
	}
	tokens := 4096
	if p.MaxOutputTokens != nil {
		tokens = int(*p.MaxOutputTokens)
	}
	return g.assistantCompletion(ctx, u, session, model, tokens, messages, nil, p.ModelBody())
}
