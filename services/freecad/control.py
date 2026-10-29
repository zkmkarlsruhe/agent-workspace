# SPDX-License-Identifier: MIT

"""Authenticated, bounded MCP facade for the Manager's application session."""
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import threading

from mcp_stdio import forward
from probe import execute

TOOLS = json.loads(Path(__file__).with_name("tools.json").read_text())
SESSION = os.environ["WORKSPACE_FREECAD_SESSION"]
_lock = threading.Lock()
_requests = {}  # retained until session end; full journal refuses new edits


def result(value, error=False):
    return {"content": [{"type": "text", "text": json.dumps(value)}],
            "structuredContent": value, "isError": error}


def validate(name, args):
    schema = next((t["inputSchema"] for t in TOOLS if t["name"] == name), None)
    if name in ("freecad_save_all", "freecad_recover_all"):
        schema = next(t["inputSchema"] for t in TOOLS if t["name"] == "freecad_save")
    if not schema or not isinstance(args, dict) or set(args) - schema["properties"].keys():
        raise ValueError("Unknown tool or argument")
    if any(k not in args for k in schema["required"]):
        raise ValueError("Required argument missing")
    for k, v in args.items():
        rule = schema["properties"][k]
        if rule["type"] == "string" and (not isinstance(v, str) or len(v) > rule.get("maxLength", 1024)):
            raise ValueError("Invalid string argument")
        if rule["type"] == "integer" and (type(v) is not int or v < rule.get("minimum", 0)):
            raise ValueError("Invalid integer argument")
        if "enum" in rule and v not in rule["enum"]:
            raise ValueError("Invalid choice")
        if "pattern" in rule and not re.fullmatch(rule["pattern"], v):
            raise ValueError("Invalid identifier")
    if args.get("session_id") != SESSION:
        raise ValueError("Session changed; reopen from the project")


def recipe(expression):
    output = execute("import json, workspace_cad; print('WORKSPACE_RESULT:' + json.dumps(" + expression + "))")
    lines = [line.removeprefix("WORKSPACE_RESULT:") for line in output.splitlines() if line.startswith("WORKSPACE_RESULT:")]
    if len(lines) != 1:
        raise RuntimeError("No complete result; inspect state before another write")
    return result(json.loads(lines[0]))


def call(name, args):
    validate(name, args)
    if name == "freecad_execute":
        key = args["request_id"]
        digest = hashlib.sha256(json.dumps(args, sort_keys=True).encode()).hexdigest()
        with _lock:
            if key in _requests:
                previous, response = _requests[key]
                if previous != digest:
                    raise ValueError("request_id was already used with different arguments")
                return response
            if len(_requests) >= 2048:
                raise ValueError("Session operation journal full; save and reopen FreeCAD")
            _requests[key] = (digest, result({"status": "outcome_unknown", "request_id": key,
                                            "message": "Do not repeat this edit; inspect state"}, True))
        code = "import json, workspace_cad; print(json.dumps(workspace_cad.edit(" + ",".join(repr(args[k]) for k in ("document", "revision", "code")) + ")))"
        response = forward({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                            "params": {"name": "execute_python", "arguments": {"code": code}}})
        if not response or "result" not in response:
            raise RuntimeError("Execution outcome unknown; inspect state")
        out = response["result"]
        page = json.loads(out["content"][0]["text"])
        out["structuredContent"] = page
        out["isError"] = bool(page.get("error")) or any(e["stream"] == "stderr" for e in page.get("page", []))
        with _lock:
            _requests[key] = (digest, out)
        return out
    if name == "freecad_output":
        response = forward({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {
            "name": "get_output_page", "arguments": {k: args[k] for k in ("job_token", "page_no")}}})
        if not response or "result" not in response:
            raise RuntimeError("Output unavailable; do not repeat the edit")
        return response["result"]
    if name == "freecad_state":
        return recipe("workspace_cad.state()")
    if name == "freecad_document":
        return recipe(f"workspace_cad.document({args['action']!r},{args['name']!r})")
    if name == "freecad_recover_all":
        return recipe("workspace_cad.recover_all()")
    if name in ("freecad_save", "freecad_save_all"):
        return recipe("workspace_cad.save_all()")
    if name == "freecad_export":
        return recipe("workspace_cad.export_object(" + ",".join(repr(args[k]) for k in ("document", "revision", "object", "format")) + ")")
    if name == "freecad_view":
        return recipe("workspace_cad.view()")
    raise ValueError("Lifecycle operation belongs to the Manager")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        if self.client_address[0] != self.server.manager_address:
            self.send_error(403)
            return
        self.connection.settimeout(10)
        try:
            token = Path("/run/freecad-token").read_text()
        except OSError:
            self.send_error(503)
            return
        if not hmac.compare_digest(self.headers.get("Authorization", "").encode(), ("Bearer " + token).encode()):
            self.send_error(401)
            return
        if self.path != "/mcp" or self.headers.get("Origin"):
            self.send_error(403)
            return
        request = None
        try:
            length = int(self.headers.get("Content-Length", 0))
            if not 0 < length <= 65536:
                raise ValueError("Invalid request size")
            request = json.loads(self.rfile.read(length))
            if not isinstance(request, dict):
                raise ValueError("Invalid request")
            if "id" not in request:
                self.send_response(202); self.end_headers(); return
            method = request.get("method")
            if method == "initialize":
                out = {"protocolVersion": "2025-11-25", "serverInfo": {"name": "freecad", "version": "1"},
                       "capabilities": {"tools": {}}, "instructions": Path(__file__).with_name("INSTRUCTIONS.md").read_text()}
            elif method == "tools/list":
                out = {"tools": [t for t in TOOLS if t["name"] not in ("freecad_open", "freecad_close")]}
            elif method == "ping":
                out = {}
            elif method == "tools/call":
                p = request.get("params", {})
                out = call(p.get("name"), p.get("arguments"))
            else:
                raise ValueError("Unknown method")
        except Exception as exc:
            out = result({"error": str(exc), "instruction": "Read state before any new write; do not retry uncertain mutations"}, True)
        body = json.dumps({"jsonrpc": "2.0", "id": request.get("id") if isinstance(request, dict) else None, "result": out}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    config = json.loads(Path("/run/freecad-control.json").read_text())
    server = ThreadingHTTPServer((config["listen"], 8080), Handler)
    server.manager_address = config["manager"]
    server.serve_forever()
