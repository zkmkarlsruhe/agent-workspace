// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// DisplayTitles returns public labels only, not a list of permitted tools. The
// web UI uses the same titles as MCP discovery; authorization remains in gate.
func DisplayTitles(lang string) map[string]string {
	ctx := withLang(context.Background(), lang)
	groups := [][]map[string]any{toolList, cadTools, officeTools, knowledgeTools,
		fileTools, nextcloudTools, nextcloudSyncTools, roomsTools,
		{mdToPDFTool, imageViewTool, imageGenTool(false), imageEditTool(), handwritingTool, feedbackTool, toolboxTool(ctx, nil)}}
	titles := map[string]string{}
	for _, group := range groups {
		for _, tool := range group {
			name, _ := tool["name"].(string)
			title, _ := tool["title"].(string)
			if name != "" && title != "" {
				titles[instance.Current().MCPServer+"_"+name] = msg(ctx, title)
			}
		}
	}
	return titles
}
