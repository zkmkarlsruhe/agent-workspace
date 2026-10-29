# SPDX-License-Identifier: MIT

"""Offline document links and basic Markdown hygiene; Python standard library only."""

import html
import os
from pathlib import Path
import re
import unicodedata
from urllib.parse import unquote, urlsplit

CODE_URL = "https://github.com/zkmkarlsruhe/agent-workspace/blob/main/"
PAGES_URL = "https://zkmkarlsruhe.github.io/agent-workspace-docs/"
# Inline links/images and reference definitions, including optional angle brackets.
LINKS = re.compile(r'(?:\]\(\s*|^ {0,3}\[[^\]\n]+\]:\s*)<?(?P<target>[^\s<>\)]+)', re.M)


def heading_ids(text):
    """Apply the default MkDocs toc slug rule to Markdown headings."""
    text = re.sub(r"(?ms)^(`{3,}|~{3,})[^\n]*\n.*?^\1[^\n]*$", "", text)
    ids = set(re.findall(r'(?i)<[a-z][^>]*\s(?:id|name)\s*=\s*["\']([^"\']+)["\']', text))
    for match in re.finditer(r"(?m)^ {0,3}#{1,6}\s+(.+?)\s*#*\s*$|^([^\n]+)\n[=-]+\s*$", text):
        heading = match[1] or match[2]
        heading = re.sub(r"!?\[([^]]+)\]\([^)]*\)", r"\1", heading)
        heading = html.unescape(re.sub(r"<[^>]*>", "", heading))
        heading = unicodedata.normalize("NFKD", heading).encode("ascii", "ignore").decode("ascii")
        slug = re.sub(r"[^\w\s-]", "", heading).strip().lower()
        slug = re.sub(r"[-\s]+", "-", slug)
        while not slug or slug in ids:
            duplicate = re.match(r"^(.*)_(\d+)$", slug)
            slug = f"{duplicate[1]}_{int(duplicate[2]) + 1}" if duplicate else slug + "_1"
        ids.add(slug)
    return ids


class DocumentChecks:
    documents_only = False
    code_root = None
    docs_root = None

    def test_document_links(self):
        code = self.code_root or os.environ.get("AW_CODE_TREE")
        docs = self.docs_root or os.environ.get("AW_DOCS_TREE")
        roots = {
            CODE_URL: Path(code) if code else (self.root.parent / "agent-workspace" if self.documents_only else self.root),
            PAGES_URL: Path(docs) if docs else (self.root if self.documents_only else self.root.parent / "agent-workspace-docs"),
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
                if not url.path and not url.fragment:
                    continue
                prefix = None
                root = self.root
                if url.scheme or url.netloc:
                    prefix = next((p for p in roots if link.startswith(p)), None)
                    if prefix is None:
                        continue
                    root = roots[prefix]
                    relative = unquote(url.path.removeprefix(urlsplit(prefix).path))
                    target = root / relative
                    if prefix == PAGES_URL:
                        root = root / "docs"
                        page = root / relative.rstrip("/")
                        target = Path(str(page) + ".md") if relative.rstrip("/") else page / "README.md"
                        if page.is_dir():
                            target = page / ("index.md" if (page / "index.md").is_file() else "README.md")
                else:
                    target = path.parent / unquote(url.path) if url.path else path
                number = text[:match.start()].count("\n") + 1
                location = f"{path.relative_to(self.root)}:{number}: {link}"
                self.assertTrue(target.resolve().is_relative_to(root.resolve()), location + " escapes tree")
                self.assertTrue(target.exists(), location + " missing target (set AW_CODE_TREE/AW_DOCS_TREE)")
                if prefix == PAGES_URL:
                    self.assertTrue(target.is_file(), location + " missing page")
                if url.fragment and target.is_file() and target.suffix == ".md":
                    self.assertIn(unquote(url.fragment), heading_ids(target.read_text()), location + " missing anchor")

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

