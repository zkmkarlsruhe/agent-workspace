# Hello MCP server

A single read-only tool, with no credentials, storage or outbound requests.
The source link is illustrative. Replace it with a real document when adapting this example.

```sh
go test ./...
go run .
```

The default listener is `127.0.0.1:8081`, endpoint `/mcp`. For a container,
bind the container interface with `-listen=0.0.0.0:8081` and do not publish the port.
This example has no authentication: restrict access to the central gateway.

See [Build and connect an MCP server](https://zkmkarlsruhe.github.io/agent-workspace-docs/mcp-server/) for registration,
the manifest's current limits, and assistant knowledge sources.

The complete opt-in overlay is in [deploy/example/mcp-hello](../../deploy/example/mcp-hello).
