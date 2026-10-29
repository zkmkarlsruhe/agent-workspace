# SPDX-License-Identifier: MIT

"""Local stdio transport to the bridge inside the visible FreeCAD process."""

import json
import sys
import urllib.error
import urllib.request

URL = "http://127.0.0.1:39280/mcp"
VERSION = "2025-11-25"


def forward(request):
    req = urllib.request.Request(
        URL,
        json.dumps(request).encode(),
        {"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
         "MCP-Protocol-Version": VERSION},
    )
    try:
        with urllib.request.urlopen(req, timeout=45) as response:
            body = response.read()
    except urllib.error.HTTPError as error:
        body = error.read()
        # Keep the bridge's JSON-RPC errors, but never pretend an HTML error is MCP.
        if not body.startswith(b"{"):
            raise RuntimeError(f"Bridge HTTP status {error.code}") from error
    return json.loads(body) if body else None


def main():
    for line in sys.stdin:
        request = None
        try:
            request = json.loads(line)
            if not isinstance(request, dict):
                raise ValueError("JSON-RPC request must be an object")
            response = forward(request)
            if response and request.get("method") == "initialize" and "result" in response:
                instructions = ("Local diagnostic bridge. Use execute_python for small GUI-thread operations and "
                                "get_output_page for all remaining pages and stderr. Read workspace_cad.state() "
                                "before changing a document. Ask for missing functional dimensions; lengths use mm. "
                                "Never repeat an uncertain mutation. This raw bridge has no project/session guards; "
                                "use the Workspace freecad_* tools for normal work.")
                response["result"]["instructions"] = instructions
        except (ValueError, OSError, RuntimeError) as error:
            response = {"jsonrpc": "2.0", "id": request.get("id") if isinstance(request, dict) else None,
                        "error": {"code": -32603,
                                  "message": f"Bridge unavailable; execution outcome unknown. Do not retry a mutation: {error}"}}
        if isinstance(request, dict) and "id" not in request:
            continue
        if response is not None:
            print(json.dumps(response), flush=True)


if __name__ == "__main__":
    main()
