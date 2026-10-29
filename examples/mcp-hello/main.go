// SPDX-License-Identifier: MIT

// mcp-hello exposes one read-only tool over streamable HTTP.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "hello", Version: "0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name: "hello", Description: "Read the example service's greeting and source link.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: "Hello from the example knowledge source. Source: https://example.org/hello",
		}}}, nil, nil
	})
	return s
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8081", "listen address; keep on an internal service network")
	flag.Parse()
	s := server()
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1 << 20}))
	httpServer := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	log.Fatal(httpServer.ListenAndServe())
}
