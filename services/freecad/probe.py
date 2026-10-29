# SPDX-License-Identifier: MIT

"""Probe actual GUI execution, rather than merely a listening HTTP server."""

import json
from mcp_stdio import forward


def execute(code):
    response = forward({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                        "params": {"name": "execute_python", "arguments": {"code": code}}})
    output = []
    for _ in range(60):
        if not response or "error" in response:
            raise RuntimeError(f"MCP failure: {response}")
        result = response["result"]
        page = json.loads(result["content"][0]["text"])
        if result.get("isError") or page.get("error"):
            raise RuntimeError(f"Execution failure: {page}")
        for entry in page["page"]:
            if entry["stream"] == "stderr":
                raise RuntimeError(entry["text"])
            output.append(entry["text"])
        if not page["has_more"]:
            return "".join(output)
        response = forward({"jsonrpc": "2.0", "id": 2, "method": "tools/call",
                            "params": {"name": "get_output_page", "arguments": {
                                "job_token": page["job_token"], "page_no": page["page_no"] + 1}}})
    raise RuntimeError("Output incomplete; do not retry the mutation")


if __name__ == "__main__":
    result = json.loads(execute("import json, workspace_cad; print(json.dumps(workspace_cad.state()))"))
    if not result["gui"] or not result["session"] or result["pid"] <= 0:
        raise RuntimeError("Visible FreeCAD process unavailable")
    print(json.dumps({key: result[key] for key in ("session", "pid", "gui", "freecad_version")}))
