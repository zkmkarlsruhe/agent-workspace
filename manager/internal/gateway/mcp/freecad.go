// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

//go:embed freecad-tools.json
var cadDefinitions []byte
var cadTools = func() []map[string]any {
	var v []map[string]any
	if err := json.Unmarshal(cadDefinitions, &v); err != nil {
		panic(err)
	}
	return v
}()

func cadTool(name string) bool {
	for _, t := range cadTools {
		if t["name"] == name {
			return true
		}
	}
	return false
}

const cadInstructions = `freecad_* opens a shared FreeCAD browser session for the current chat project. Call freecad_open; poll while starting, then give browser_url to the person to open in a new tab. Keep session_id. Read freecad_state before every change. Use freecad_document to create or open a native document. freecad_execute takes document, fresh revision, Python code and a unique request_id. App, Gui and doc are available; use native FreeCAD parameters and small operations. Names and revisions come from state, never guess them. Ask for missing functional dimensions; lengths are mm. Preserve human changes. A conflict requires reading again. When has_more is true use freecad_output; never repeat an uncertain write. Read stderr and all pages; MCP transport success is not CAD success. freecad_save saves native files; freecad_export exports an explicitly selected solid. freecad_view returns a project PNG path for image_view. freecad_close saves and stops the session. Do not close documents or modify files outside this project in Python. A project switch saves and closes the previous application. Native files and periodic saves remain in the project's freecad folder.`

func (s *Server) freecadCall(ctx context.Context, u store.User, session, name string, args json.RawMessage) (toolResult, callMeta) {
	meta := callMeta{capability: "freecad", target: name, status: http.StatusOK}
	result, err := s.FreeCAD(ctx, u.UID, session, name, args)
	if err != nil {
		meta.status = http.StatusBadGateway
		return errorResult(ctx, "FreeCAD request failed. A write may have run; read the current state before another edit. Reopen FreeCAD from this project if the session ended."), meta
	}
	var out toolResult
	data, err := json.Marshal(result)
	if err == nil {
		err = json.Unmarshal(data, &out)
	}
	if err != nil {
		meta.status = http.StatusBadGateway
		return errorResult(ctx, "FreeCAD returned an invalid response."), meta
	}
	return out, meta
}
