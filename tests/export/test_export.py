# SPDX-License-Identifier: MIT

"""Checks for the public source archive; no network or installed services needed."""

import json
from pathlib import Path
import re
import runpy
import unittest

ROOT = Path(__file__).resolve().parents[2]
CHECKS = runpy.run_path(str(Path(__file__).with_name("semantic_checks.py")))
DOCUMENTS = runpy.run_path(str(Path(__file__).with_name("document_checks.py")))
NOTICE = (
    "Prototype for evaluation only. Not production software, no support, "
    "no guarantees. Do not make it reachable from the internet; run it only in an "
    "internal network. Use at your own risk."
)


class ExportTest(DOCUMENTS["DocumentChecks"], unittest.TestCase):
    root = ROOT

    def test_boundary(self):
        forbidden = {"research", "DECISIONS.md", "IDEAS.md", "INVENTORY-house.md",
                     "scrub-check", "scrub-names.txt", "scrub-allowlist.json",
                     "export-include.txt", "export-exclude.txt", "public-allowlist.json", "public-docs-allowlist.json"}
        for path in ROOT.rglob("*"):
            relative = path.relative_to(ROOT)
            if ".git" in relative.parts:
                continue
            self.assertFalse(path.is_symlink(), relative)
            self.assertFalse(forbidden.intersection(relative.parts), relative)
            self.assertFalse(path.name.startswith("PLAN-"), relative)
            self.assertNotIn(path.suffix.lower(), {".ttf", ".otf", ".woff", ".woff2"}, relative)
        module = (ROOT / "manager/go.mod").read_text().splitlines()[0]
        self.assertEqual(module, "module github.com/zkmkarlsruhe/agent-workspace/manager")

    def test_notices_and_metadata(self):
        for name in ("README.md", "docs/README.md", "docs/INSTALL.md"):
            self.assertIn(NOTICE, (ROOT / name).read_text())
        for name in ("codemeta.json", ".zenodo.json"):
            data = json.loads((ROOT / name).read_text())
            self.assertIn("MIT", data["license"])
            self.assertNotIn("grants", data)
        self.assertIn("MIT License", (ROOT / "LICENSE").read_text())

    def test_instance_defaults_and_rendered_assets(self):
        minimal = (ROOT / "instance/minimal/instance.yaml").read_text()
        example = (ROOT / "instance/example/instance.yaml").read_text()
        self.assertEqual(example, (ROOT / "manager/internal/instance/example.yaml").read_text())
        for section in ("values", "defaults"):
            block = re.compile(r"^" + section + r":\n(?:[ \t].*\n|\n)*", re.M)
            for line in block.search(minimal)[0].splitlines():
                self.assertIn(line, block.search(example)[0].splitlines())
        self.assertEqual(minimal, (ROOT / "manager/internal/instance/minimal.yaml").read_text())
        rendered = ROOT / "instance/minimal/rendered"
        for path in rendered.rglob("*"):
            if path.is_file():
                self.assertEqual(path.read_bytes(),
                                 (ROOT / "deploy/etc-opencode" / path.relative_to(rendered)).read_bytes())

    def test_no_default_instance(self):
        paths = list((ROOT / "instance").rglob("*.yaml"))
        paths += list((ROOT / "manager/internal/instance").glob("*.yaml"))
        for path in paths:
            self.assertNotIn("default: true", path.read_text(), path.relative_to(ROOT))

    def test_no_unbuildable_media_image(self):
        catalog = json.loads((ROOT / "deploy/catalog/mcp.json").read_text())
        self.assertNotIn("media", [item["id"] for item in catalog["items"]])
        for item in catalog["items"]:
            self.assertNotIn("sha256:", item.get("image", ""), item["id"])

    def test_hello_overlay(self):
        overlay = ROOT / "deploy/example/mcp-hello"
        for name in ("compose.yaml", "litellm.yaml", "catalog.json", "instance.yaml", "integrations/hello.json"):
            self.assertTrue((overlay / name).is_file(), name)
        manifest = json.loads((overlay / "integrations/hello.json").read_text())
        self.assertEqual(manifest, json.loads((ROOT / "examples/mcp-hello/manifest.json").read_text()))
        self.assertEqual(manifest["kind"], "remote")
        self.assertNotIn("enabled_by_default", manifest)
        item, = json.loads((overlay / "catalog.json").read_text())["items"]
        self.assertEqual((item["id"], item["litellm"], item["tools"]), ("hello", ["hello"], ["hello"]))
        self.assertIn("OC_INTEGRATIONS_DIR: /integrations", (overlay / "compose.yaml").read_text())
        self.assertIn("server_id: hello", (overlay / "litellm.yaml").read_text())
        self.assertIn("allow_all_keys: false", (overlay / "litellm.yaml").read_text())
        self.assertIn("FROM golang:", (ROOT / "examples/mcp-hello/Dockerfile").read_text())
        for directory in (overlay, ROOT / "examples/mcp-hello"):
            for path in directory.rglob("*"):
                if path.is_file():
                    self.assertNotRegex(path.read_text(), r"sha256:[0-9a-f]{64}")
                    for image in re.findall(r"(?:image:|FROM)\s+(\S+)", path.read_text()):
                        self.assertIn(image, {"workspace-mcp-hello:local", "golang:1.27", "scratch"})
        self.assertNotIn("image", manifest)
        self.assertNotIn("hello: true", (ROOT / "instance/minimal/instance.yaml").read_text())

    def test_content_references(self):
        forbidden = re.compile(r"HANDOFF|research/0[0-9]|blueprint/|TODO\(")
        for path in ROOT.rglob("*"):
            relative = path.relative_to(ROOT).as_posix()
            if path.suffix not in {".yaml", ".jsonc", ".md", ".go"} or relative == "docs/adr/README.md":
                continue
            for number, line in enumerate(path.read_text().splitlines(), 1):
                self.assertIsNone(forbidden.search(line), f"{relative}:{number}: {line}")


class SemanticExportTest(CHECKS["SemanticChecks"], unittest.TestCase):
    root = ROOT

    def test_private_integrations_are_not_selected(self):
        builtin = json.loads((self.root / "manager/internal/integration/builtins.json").read_text())
        for name in ("library", "workspace-knowledge"):
            self.assertNotIn(name, [m["name"] for m in builtin])
            self.assertFalse((self.root / "deploy/integrations" / name).exists())
        for name in ("example", "minimal"):
            path = self.root / f"instance/{name}/instance.yaml"
            self.assertNotRegex(path.read_text(), r"(?m)^  (?:library|workspace-knowledge): (?:true|false)$")

    def test_two_instances_render_in_isolation(self):
        import hashlib
        import os
        import shutil
        import subprocess
        import tempfile

        go = os.environ.get("GO") or shutil.which("go") or str(Path.home() / ".local/go/bin/go")
        env = dict(os.environ, GOPROXY="off", GOSUMDB="off", GOTOOLCHAIN="local")
        env.pop("OC_INTEGRATIONS_DIR", None)
        with tempfile.TemporaryDirectory(prefix="two-instances-") as temp:
            temp = Path(temp)
            binary = temp / "render-instance"
            subprocess.run([go, "build", "-buildvcs=false", "-o", str(binary), "./cmd/render-instance"],
                           cwd=self.root / "manager", env=env, check=True)
            outputs = []
            markers = []
            for base, flavor, provider, price in (
                    ("minimal", "openai", "NorthProvider", "7.12345"),
                    ("example", "litellm", "SouthProvider", "9.54321")):
                source = self.root / f"instance/{base}/instance.yaml"
                text = source.read_text()
                host = provider.lower() + ".example.test"
                title = f"Reviewed consent for {provider}"
                point = f"Requests go only to {provider} at {host}."
                consent_hash = hashlib.sha256((title + "\n" + point).encode()).hexdigest()[:12]
                # Replace complete top-level blocks; JSON scalars are valid YAML, no YAML dependency needed.
                blocks = {
                    "models": f"models:\n  default: {provider}\n  small: {provider}-small\n  thorough: {provider}-thorough\n",
                    "accounting": f"accounting:\n  upstream_cost_currency: USD\n  usd_per_eur: 1\n  largest_context: 262144\n  max_output_tokens: 32768\n  prices:\n    standard: {{input: {price}, output: {price}}}\n    fast: {{input: {price}, output: {price}}}\n    thorough: {{input: {price}, output: {price}}}\n",
                    "consent": f"consent:\n  version: {consent_hash}\n" + "".join(
                        f"  {lang}:\n    title: {title}\n    short: {consent_hash}\n    points:\n    - {point}\n" for lang in ("en", "de")),
                }
                for key, block in blocks.items():
                    text, count = re.subn(r"^" + key + r":\n(?:[ \t].*\n|\n)*", lambda _: block, text, flags=re.M)
                    self.assertEqual(count, 1, f"{source}:1: missing {key} block")
                text, count = re.subn(r"(?m)^  domain:.*$", "  domain: " + host, text)
                self.assertEqual(count, 1)
                text, count = re.subn(r"(?m)^  OC_DOMAIN:.*$", "  OC_DOMAIN: " + host, text)
                self.assertEqual(count, 1)
                source = temp / f"{base}.yaml"
                source.write_text(text)
                out = temp / base
                command = [str(binary), "-instance", str(source), "-out", str(out)]
                run_env = dict(env, OC_LLM_UPSTREAM_FLAVOR=flavor)
                subprocess.run(command, env=run_env, check=True)
                subprocess.run(command + ["-check"], env=run_env, check=True)
                rendered = {str(p.relative_to(out)): p.read_text() for p in sorted(out.rglob("*")) if p.is_file()}
                self.assertTrue(rendered)
                for lang in ("en", "de"):
                    notice = rendered[f"consent.{lang}.md"].splitlines()
                    payload = notice[0].removeprefix("# ") + "\n" + "\n".join(s[2:] for s in notice if s.startswith("- "))
                    self.assertEqual(hashlib.sha256(payload.encode()).hexdigest()[:12], consent_hash)
                print(f"two-instance {base}/{flavor} scanned: " + ", ".join(rendered))
                markers.append((provider, price, consent_hash, host))
                outputs.append(rendered)
            for index, rendered in enumerate(outputs):
                joined = "\n".join(rendered.values())
                for marker in markers[index]:
                    self.assertIn(marker, joined, f"instance {index}: missing positive control {marker}")
                for path, text in rendered.items():
                    for number, line in enumerate(text.splitlines(), 1):
                        self.assertNotRegex(line, r"\[\[[a-z_][a-z0-9_.:-]*\]\]", f"{path}:{number}: unresolved token")
                        for marker in markers[1 - index]:
                            self.assertNotIn(marker, line, f"{path}:{number}: cross-instance {marker}")


if __name__ == "__main__":
    unittest.main()
