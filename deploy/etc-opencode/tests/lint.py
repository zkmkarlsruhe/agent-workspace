# SPDX-License-Identifier: MIT

"""Lint for deploy/etc-opencode (docs/adr/README.md §4.2). Needs PyYAML. Exit code 1 on any finding.

Usage: python3 lint.py deploy/etc-opencode [deploy/catalog [path/to/opencode.jsonc]]
"""
import json
import re
import sys
from pathlib import Path

import yaml

ROOT = Path(sys.argv[1])
# own skills of the person: my-* (and meine-* from before the rename, kept valid for good)
RESERVED = ("my-", "meine-")
OLD_KEYS = {"workspace/titel", "workspace/titel-en", "workspace/beschreibung-en", "workspace/stufe", "workspace/kategorie", "workspace/quelle"}
ALLOWED = {
    "agents": {"model", "request", "system", "description", "mode", "hidden", "color", "steps", "disabled", "permissions", "variant"},
    "commands": {"description", "agent", "model", "subagent"},
    "skills": {"name", "description", "metadata", "disable-model-invocation"},
}
ACTIONS = {"provider.use", "tool.use", "integration.use"}
# U+2460-24FF enclosed alphanumerics, U+2776-2793 dingbat digits, U+1D400+ math alphanumerics, U+1F000+ emoji
GIMMICK = re.compile("[%c-%c%c-%c%c-%c%c-%c]" % (0x2460, 0x24FF, 0x2776, 0x2793, 0x1D400, 0x1D7FF, 0x1F000, 0x1FAFF))
errors = []


def strip_jsonc(text: str) -> str:
    # drops // comments outside strings; enough for this file (no /* */ and no trailing commas)
    out, i, in_str = [], 0, False
    while i < len(text):
        ch = text[i]
        if in_str:
            out.append(ch)
            if ch == "\\":
                out.append(text[i + 1])
                i += 1
            elif ch == '"':
                in_str = False
        elif ch == '"':
            in_str = True
            out.append(ch)
        elif text.startswith("//", i):
            while i < len(text) and text[i] != "\n":
                i += 1
            continue
        else:
            out.append(ch)
        i += 1
    return "".join(out)


cfg_text = (Path(sys.argv[3]) if len(sys.argv) > 3 else ROOT / "opencode.jsonc").read_text()
if not cfg_text.startswith("// release: "):
    errors.append("opencode.jsonc: first line must be '// release: <tag> <date>'")
cfg = json.loads(strip_jsonc(cfg_text))
# The web UI shows a model unasked only if it is the newest of its family; missing families share one group.
for pid, prov in cfg.get("providers", {}).items():
    fams = [m.get("family") for m in prov.get("models", {}).values()]
    if None in fams or len(set(fams)) != len(fams):
        errors.append(f"provider {pid}: every model needs its own 'family', else the model picker hides choices")
for n, p in enumerate(cfg.get("policies", []), 1):
    if p.get("effect") not in ("allow", "deny") or p.get("action") not in ACTIONS or set(p) != {"action", "resource", "effect"}:
        errors.append(f"policy {n} invalid: {p}")

for kind, keys in ALLOWED.items():
    files = sorted((ROOT / kind).glob("*/SKILL.md" if kind == "skills" else "*.md"))
    if not files:
        errors.append(f"{kind}: no files")
    for f in files:
        text = f.read_text()
        m = re.match(r"^---\n(.*?)\n---\n", text, re.S)
        if not m:
            errors.append(f"{f}: no frontmatter")
            continue
        fm = yaml.safe_load(m.group(1)) or {}
        if extra := set(fm) - keys:
            errors.append(f"{f}: keys not allowed: {sorted(extra)}")
        if not fm.get("description"):
            errors.append(f"{f}: description missing")
        if kind == "skills":
            if fm.get("name") != f.parent.name:
                errors.append(f"{f}: name must equal folder name")
            if len(fm["description"]) > 400:
                errors.append(f"{f}: description > 400 chars")
            if not (fm.get("metadata") or {}).get("workspace/version"):
                errors.append(f"{f}: metadata.workspace/version missing")
            if f.parent.name.startswith(RESERVED) and f.parent.name != "my-skills":
                errors.append(f"{f}: prefixes my- and meine- are reserved for users")
            md = fm.get("metadata") or {}
            if "Load for " not in fm["description"]:
                errors.append(f"{f}: description needs trigger words after 'Load for '")
            for k in OLD_KEYS & set(md):
                errors.append(f"{f}: metadata.{k} is a pre-rename key, see docs/RENAME-ENGLISH.md 2d")
            if len([p for p in f.parent.rglob("*") if p.is_file()]) > 11:
                errors.append(f"{f.parent}: more than 10 files besides SKILL.md")

CATALOG = Path(sys.argv[2]) if len(sys.argv) > 2 else None
ID = re.compile(r"^[a-z0-9-]{2,40}$")
PINNED = re.compile(r"^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$")
# category ids; the portal labels them (de: Texte, Tabellen, Präsentation, Daten, Bilder, Recherche, Organisation, Workspace-Wissen)
CATEGORIES = {"texts", "spreadsheets", "presentations", "data", "images", "research", "organisation", "workspace-knowledge"}
TIERS = ("optional", "standard")
LANG_KEY = re.compile(r"^(title|description)_[a-z]{2}$")


def catalog_lint(root: Path, base_ids: set) -> None:
    ids = set()
    for f in sorted((root / "skills").glob("*/SKILL.md")):
        sid = f.parent.name
        m = re.match(r"^---\n(.*?)\n---\n", f.read_text(), re.S)
        if not m:
            errors.append(f"{f}: no frontmatter")
            continue
        try:
            fm = yaml.safe_load(m.group(1)) or {}
        except yaml.YAMLError as e:
            errors.append(f"{f}: frontmatter is not valid YAML: {e}".replace("\n", " "))
            continue
        if extra := set(fm) - {"name", "description", "metadata"}:
            errors.append(f"{f}: keys not allowed: {sorted(extra)}")
        md = fm.get("metadata") or {}
        if not ID.match(sid) or sid.startswith(RESERVED):
            errors.append(f"{f}: id must match ^[a-z0-9-]{{2,40}}$ and not start with my- or meine-")
        if fm.get("name") != sid:
            errors.append(f"{f}: name must equal folder name")
        if not isinstance(fm.get("description"), str) or not fm["description"].strip() or len(fm["description"]) > 400:
            errors.append(f"{f}: description missing or > 400 chars")
        if sid in base_ids:
            errors.append(f"{f}: id is also a base skill")
        if any(not isinstance(v, str) for v in md.values()):
            errors.append(f"{f}: metadata values must be quoted strings")
        if not re.match(r"^\d+\.\d+\.\d+$", str(md.get("workspace/version", ""))):
            errors.append(f"{f}: metadata.workspace/version must be SemVer")
        if md.get("workspace/tier") not in TIERS:
            errors.append(f"{f}: metadata.workspace/tier must be optional or standard")
        if not md.get("workspace/title") or len(str(md["workspace/title"])) > 40:
            errors.append(f"{f}: metadata.workspace/title missing or > 40 chars")
        if not md.get("workspace/title-de") or len(str(md["workspace/title-de"])) > 40 or not md.get("workspace/description-de") or len(str(md["workspace/description-de"])) > 300:
            errors.append(f"{f}: metadata.workspace/title-de (<= 40) or workspace/description-de (<= 300) missing")
        if md.get("workspace/category") not in CATEGORIES:
            errors.append(f"{f}: metadata.workspace/category must be one of {sorted(CATEGORIES)}")
        for k in ("workspace/owner", "workspace/license"):
            if not md.get(k):
                errors.append(f"{f}: metadata.{k} missing")
        files = [p for p in f.parent.rglob("*") if not p.is_dir()]
        if any(p.is_symlink() for p in files):
            errors.append(f"{f.parent}: symlinks not allowed")
        if len(files) > 11:
            errors.append(f"{f.parent}: more than 10 files besides SKILL.md")
        if [p for p in f.parent.glob("*.md") if p.name != "SKILL.md"]:
            errors.append(f"{f.parent}: .md besides SKILL.md only in subfolders")
        if [p for p in f.parent.rglob("test_*.py")]:
            errors.append(f"{f.parent}: tests belong in tests/<id>/, not in the skill")
        ids.add(sid)
    for d in sorted((root / "skills").iterdir()) if (root / "skills").is_dir() else []:
        if d.is_dir() and not (d / "SKILL.md").is_file():
            errors.append(f"{d}: no SKILL.md")
    try:
        items = json.loads((root / "mcp.json").read_text())["items"]
    except (OSError, ValueError, KeyError) as e:
        errors.append(f"{root}/mcp.json: {e}")
        items = []
    for it in items:
        iid = it.get("id", "")
        kind = it.get("kind", "mcp")
        extra = {"mcp": {"server"}, "mcp-central": {"litellm"}, "toolbox": {"image", "programs"}}.get(kind)
        if extra is None:
            errors.append(f"mcp.json {iid}: kind must be mcp, mcp-central or toolbox")
            extra = set()
        if {k for k in set(it) - {"id", "kind", "title", "description", "tier", "category", "tools"} - extra if not LANG_KEY.match(k)}:
            errors.append(f"mcp.json {iid}: unknown keys")
        if not ID.match(iid) or iid.startswith(RESERVED) or iid in ids or iid in base_ids:
            errors.append(f"mcp.json {iid}: id invalid or not unique across skills and base skills")
        ids.add(iid)
        if it.get("tier") not in TIERS or it.get("category") not in CATEGORIES:
            errors.append(f"mcp.json {iid}: tier or category invalid")
        if not it.get("title") or len(it["title"]) > 40 or not it.get("description") or len(it["description"]) > 300:
            errors.append(f"mcp.json {iid}: title (<= 40) or description (<= 300) missing")
        if not it.get("title_de") or len(it["title_de"]) > 40 or not it.get("description_de") or len(it["description_de"]) > 300:
            errors.append(f"mcp.json {iid}: title_de (<= 40) or description_de (<= 300) missing")
        strs = lambda k: bool(it.get(k)) and isinstance(it[k], list) and all(isinstance(t, str) and t for t in it[k])
        if kind == "mcp" and (it.get("server", "workspace") != "workspace" or not strs("tools")):
            errors.append(f"mcp.json {iid}: server must be workspace and tools a non-empty list")
        if kind == "mcp-central":
            # D59: opt-in only; tools is the allowlist, the admin policy must allow the server by name
            if it.get("tier") != "optional" or not strs("tools") or not strs("litellm"):
                errors.append(f"mcp.json {iid}: mcp-central needs tier optional, litellm and tools (allowlist)")
            if f'"resource": "mcp:{iid}", "effect": "allow"' not in cfg_text:
                errors.append(f"mcp.json {iid}: no integration.use allow for mcp:{iid} in opencode.jsonc")
        if kind == "toolbox" and (it.get("tier") != "optional" or "tools" in it or not strs("programs")
                                  or not PINNED.match(it.get("image", ""))):
            errors.append(f"mcp.json {iid}: toolbox needs tier optional, programs and image as repo:tag@sha256:<64 hex>")
    for f in root.rglob("*"):
        if f.is_file() and f.suffix in {".md", ".json", ".py"} and GIMMICK.search(f.read_text()):
            errors.append(f"{f}: decorative unicode")


if CATALOG:
    catalog_lint(CATALOG, {p.name for p in (ROOT / "skills").iterdir() if p.is_dir()})

for f in ROOT.rglob("*"):
    if f.is_file() and f.suffix in {".md", ".jsonc", ".py"} and GIMMICK.search(f.read_text()):
        errors.append(f"{f}: decorative unicode")

print("\n".join(errors) or "lint ok")
sys.exit(1 if errors else 0)
