# SPDX-License-Identifier: MIT

"""Offline document links and basic Markdown hygiene; Python standard library only."""

import os
from pathlib import Path
import re
from urllib.parse import unquote, urlsplit

CODE_URL = "https://github.com/zkmkarlsruhe/agent-workspace/blob/main/"
DOCS_URL = "https://github.com/zkmkarlsruhe/agent-workspace-docs/blob/main/"
# Inline links/images and reference definitions, including optional angle brackets.
LINKS = re.compile(r'(?:\]\(\s*|^ {0,3}\[[^\]\n]+\]:\s*)<?(?P<target>[^\s<>\)]+)', re.M)


class DocumentChecks:
    documents_only = False
    code_root = None
    docs_root = None

    def test_document_links(self):
        code = self.code_root or os.environ.get("AW_CODE_TREE")
        docs = self.docs_root or os.environ.get("AW_DOCS_TREE")
        roots = {
            CODE_URL: Path(code) if code else (self.root.parent / "agent-workspace" if self.documents_only else self.root),
            DOCS_URL: Path(docs) if docs else (self.root if self.documents_only else self.root.parent / "agent-workspace-docs"),
        }
        paths = sorted(self.root.rglob("*.md"))
        self.assertTrue(paths, "No documentation scanned")
        for path in paths:
            if ".git" in path.relative_to(self.root).parts:
                continue
            text = path.read_text()
            for match in LINKS.finditer(text):
                link = match["target"]
                url = urlsplit(link)
                if link.startswith("#") or not url.path:
                    continue
                root = self.root
                if url.scheme or url.netloc:
                    prefix = next((p for p in roots if link.startswith(p)), None)
                    if prefix is None:
                        continue
                    root = roots[prefix]
                    target = root / unquote(url.path.removeprefix(urlsplit(prefix).path))
                else:
                    target = path.parent / unquote(url.path)
                number = text[:match.start()].count("\n") + 1
                location = f"{path.relative_to(self.root)}:{number}: {link}"
                self.assertTrue(target.resolve().is_relative_to(root.resolve()), location + " escapes tree")
                self.assertTrue(target.exists(), location + " missing target (set AW_CODE_TREE/AW_DOCS_TREE)")

    def test_markdown(self):
        paths = sorted(self.root.rglob("*.md"))
        self.assertTrue(paths, "No Markdown scanned")
        for path in paths:
            if ".git" in path.relative_to(self.root).parts:
                continue
            text = path.read_text(encoding="utf-8")
            self.assertTrue(text.endswith("\n"), f"{path.relative_to(self.root)}: missing final newline")
            self.assertNotIn("\x00", text, str(path.relative_to(self.root)))
            self.assertNotRegex(text, r"(?m)^(?:<<<<<<< |=======\s*$|>>>>>>> )",
                                f"{path.relative_to(self.root)}: unresolved conflict")

    def test_license(self):
        text = (self.root / "LICENSE").read_text()
        for notice in ("MIT License", "Permission is hereby granted, free of charge",
                       "The above copyright notice and this permission notice",
                       'THE SOFTWARE IS PROVIDED "AS IS"'):
            self.assertIn(notice, text)
        if self.documents_only:
            code = self.code_root or os.environ.get("AW_CODE_TREE") or self.root.parent / "agent-workspace"
            self.assertEqual(text, (Path(code) / "LICENSE").read_text())
            for name in ("CITATION.cff", "codemeta.json", ".zenodo.json"):
                self.assertFalse((self.root / name).exists(), name + " belongs to the code release")

    def test_spdx_headers(self):
        paths = sorted(p for p in self.root.rglob("*") if p.suffix in {".go", ".py"}
                       and ".git" not in p.parts)
        self.assertTrue(paths, "no exported sources scanned")
        for path in paths:
            with self.subTest(path=str(path.relative_to(self.root))):
                lines = path.read_text().splitlines()
                if lines and lines[0].startswith("#!"):
                    lines = lines[1:]
                prefix = "//" if path.suffix == ".go" else "#"
                if lines and lines[0].startswith(prefix + " Code generated ") and lines[0].endswith(" DO NOT EDIT."):
                    continue
                self.assertGreaterEqual(len(lines), 2)
                self.assertEqual(lines[0], prefix + " SPDX-License-Identifier: MIT")
                self.assertEqual(lines[1], "")

