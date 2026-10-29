// SPDX-License-Identifier: MIT

//go:build mcpharness

package mcp

import (
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
)

// TestHarness serves the Workspace-MCP for a real OpenCode container on a slot
// network (slot 1 = test user), e.g. as llm-gw at 10.90.1.3:4000:
// go test -tags mcpharness -run TestHarness -timeout 0 ./internal/gateway/mcp/
func TestHarness(t *testing.T) {
	s := New(Server{SearXNG: mustURL(os.Getenv("OC_SEARXNG_URL")), SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf,
		Users: users{}, Usage: &rec{}, Log: slog.New(slog.NewJSONHandler(os.Stdout, nil))})
	mux := http.NewServeMux()
	mux.Handle("/mcp/workspace", s)
	t.Fatal(http.ListenAndServe(":4000", mux))
}
