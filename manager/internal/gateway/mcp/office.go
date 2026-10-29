// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// officeDefinitions is exported from nextcloud-browser-bridge v0.4.0 tools/list.
//
//go:embed office-tools.json
var officeDefinitions []byte
var officeTools = func() []map[string]any {
	var tools []map[string]any
	if err := json.Unmarshal(officeDefinitions, &tools); err != nil {
		panic(err)
	}
	return tools
}()

func officeTool(name string) bool {
	for _, tool := range officeTools {
		if tool["name"] == name {
			return true
		}
	}
	return false
}

func (s *Server) officeCall(ctx context.Context, u store.User, name string, args json.RawMessage) (toolResult, callMeta) {
	meta := callMeta{capability: "office", target: name, status: http.StatusOK}
	result, err := s.Office(ctx, u.UID, name, args)
	if err != nil {
		meta.status = http.StatusBadGateway
		if name == "office_replace" {
			return errorResult(ctx, "Office write outcome is uncertain. Read the document before any new edit; do not repeat this write automatically."), meta
		}
		return errorResult(ctx, "Office is unavailable. Check your Nextcloud connection and reopen the document."), meta
	}
	var out toolResult
	data, err := json.Marshal(result)
	if err == nil {
		err = json.Unmarshal(data, &out)
	}
	if err != nil {
		meta.status = http.StatusBadGateway
		return errorResult(ctx, "Office returned an invalid response."), meta
	}
	return out, meta
}
