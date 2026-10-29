// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHello(t *testing.T) {
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := server().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 1 || list.Tools[0].Name != "hello" || !list.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tool discovery: %+v, %v", list, err)
	}
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "hello", Arguments: map[string]any{}})
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("tool call: %+v, %v", result, err)
	}
	if text, ok := result.Content[0].(*mcp.TextContent); !ok || !strings.Contains(text.Text, "https://example.org/hello") {
		t.Fatalf("missing source: %+v", result.Content)
	}
	result, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "missing"})
	if err == nil && !result.IsError {
		t.Fatal("unknown tool accepted")
	}
}
