# SPDX-License-Identifier: MIT

"""Negative and positive test of the catalog part of deploy/etc-opencode/tests/lint.py. Needs PyYAML.

Usage: python3 -I -B deploy/catalog/tests/test_lint.py
"""
import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

DEPLOY = Path(__file__).resolve().parents[2]
LINT = DEPLOY / "etc-opencode" / "tests" / "lint.py"

GOOD = """---
name: good
description: "Good: with a colon in quotes. Load for good, gut."
metadata:
  workspace/version: "1.0.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/tier: "optional"
  workspace/title: "Good"
  workspace/title-de: "Gut"
  workspace/description-de: "Gut: mit Doppelpunkt in Anführungszeichen."
  workspace/category: "texts"
---
# Good
"""

BAD = {
    # unquoted ": " breaks YAML (the agent-C finding)
    "broken": "---\nname: broken\ndescription: broken: because of the colon\nmetadata:\n  workspace/version: \"1.0.0\"\n---\n",
    # reserved prefix, unquoted metadata, no catalog fields
    "my-x": "---\nname: my-x\ndescription: x\nmetadata:\n  workspace/version: 1\n---\n",
    # the old own-skill prefix stays reserved
    "meine-x": GOOD.replace("name: good", "name: meine-x"),
    # same id as a base skill
    "pdf": GOOD.replace("name: good", "name: pdf"),
    # pre-rename keys
    "old-keys": GOOD.replace("name: good", "name: old-keys").replace("workspace/tier: \"optional\"", "workspace/stufe: \"katalog\""),
}


def lint(root: Path) -> subprocess.CompletedProcess:
    with tempfile.TemporaryDirectory() as d:
        base = Path(d) / "etc-opencode"
        shutil.copytree(DEPLOY / "etc-opencode", base)
        # Keep fixture policies independent of the deployment's enabled integrations.
        policy = {"action": "integration.use", "resource": "mcp:hello", "effect": "allow"}
        (base / "opencode.jsonc").write_text("// release: fixture\n" + json.dumps({"policies": [policy]}))
        return subprocess.run([sys.executable, "-I", "-B", str(LINT), str(base), str(root)],
                              capture_output=True, text=True)


def write(root: Path, skills: dict, items: list) -> None:
    for sid, text in skills.items():
        (root / "skills" / sid).mkdir(parents=True)
        (root / "skills" / sid / "SKILL.md").write_text(text)
    (root / "mcp.json").write_text(json.dumps({"items": items}))


ITEM = {"id": "md-to-pdf", "title": "PDF", "title_de": "PDF", "description": "x", "description_de": "x", "tier": "optional",
        "category": "texts", "server": "workspace", "tools": ["md_to_pdf"]}
CENTRAL = dict(ITEM, id="hello", kind="mcp-central", litellm=["hello"], tools=["hello"])
del CENTRAL["server"]

with tempfile.TemporaryDirectory() as d:
    root = Path(d)
    write(root, {"good": GOOD}, [ITEM, CENTRAL])
    r = lint(root)
    assert r.returncode == 0, r.stdout

with tempfile.TemporaryDirectory() as d:
    root = Path(d)
    toolbox = {k: v for k, v in CENTRAL.items() if k not in ("litellm", "tools")}
    toolbox.update(kind="toolbox", programs=["ffmpeg"], image="registry.example.org/operations/opencode-tool-medien:1@sha256:" + "0" * 64)
    write(root, {"good": GOOD, **BAD}, [ITEM, dict(ITEM, id="good"), CENTRAL,
                                        dict(CENTRAL, id="no-policy"), dict(CENTRAL, id="central-std", tier="standard"),
                                        dict(toolbox, id="media"), dict(toolbox, id="no-digest", image="registry.example.org/x:latest"),
                                        dict(toolbox, id="foreign", kind="plugin"), dict(ITEM, id="no-de", title_de=""),
                                        dict(ITEM, id="old-tier", tier="katalog"), dict(ITEM, id="my-item"),
                                        dict(ITEM, id="old-key", titel="x")])
    (root / "skills" / "good" / "test_x.py").write_text("")
    out = lint(root).stdout
    for want in ["broken/SKILL.md: frontmatter is not valid YAML", "my-x/SKILL.md: id must match",
                 "meine-x/SKILL.md: id must match",
                 "my-x/SKILL.md: metadata values must be quoted", "pdf/SKILL.md: id is also a base skill",
                 "old-keys/SKILL.md: metadata.workspace/tier must be",
                 "good: tests belong in tests/", "mcp.json good: id invalid or not unique",
                 "mcp.json no-policy: no integration.use allow", "mcp.json central-std: mcp-central needs tier optional",
                 "mcp.json no-digest: toolbox needs", "mcp.json foreign: kind must be",
                 "my-x/SKILL.md: metadata.workspace/title-de", "mcp.json no-de: title_de",
                 "mcp.json old-tier: tier or category invalid", "mcp.json my-item: id invalid",
                 "mcp.json old-key: unknown keys"]:
        assert want in out, (want, out)
    for ok in ["mcp.json hello", "mcp.json media"]:
        assert ok not in out, (ok, out)

print("test_lint ok")
