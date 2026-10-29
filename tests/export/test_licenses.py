# SPDX-License-Identifier: MIT

"""Source licence headers, module policy and report freshness."""

import difflib
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


class LicenseTest(unittest.TestCase):
    root = ROOT

    def script(self):
        return runpy.run_path(str(self.root / "scripts/license-report"))

    def test_license_report_current(self):
        report = self.root / "THIRD_PARTY_LICENSES.md"
        self.assertTrue(report.is_file(), "THIRD_PARTY_LICENSES.md missing")
        with tempfile.TemporaryDirectory() as temp:
            regenerated = Path(temp) / "report.md"
            subprocess.run([sys.executable, "-I", "-B", str(self.root / "scripts/license-report"),
                            "--go", os.environ.get("GO", shutil.which("go") or str(Path.home() / ".local/go/bin/go")),
                            "--strict", "--output", str(regenerated)], check=True)
            before, after = report.read_text(), regenerated.read_text()
            self.assertEqual(before, after, "".join(difflib.unified_diff(
                before.splitlines(True), after.splitlines(True), fromfile=str(report), tofile="regenerated")))

    def test_python_services_inventory_fails_closed(self):
        inventory = self.script()["python_services"]
        rows = inventory(self.root)
        self.assertEqual(len(rows), 9)
        self.assertTrue(any(name == "CairoSVG" and "LGPL" in licence for _, name, _, licence in rows))
        report = (self.root / "THIRD_PARTY_LICENSES.md").read_text()
        for component in ("ghcr.io/linuxserver/freecad", "python:3.14-slim", "rembg", "pypdfium2"):
            self.assertIn(component, report)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            path = root / "services/new/requirements.txt"
            path.parent.mkdir(parents=True)
            path.write_text("unreviewed==1.0\n")
            with self.assertRaisesRegex(ValueError, "unreviewed"):
                inventory(root)

    def test_unknown_base_is_listed_and_only_strict_cli_fails(self):
        script = self.script()
        original_read = Path.read_text
        dockerfile = self.root / "services/freecad/Dockerfile"
        def read(path, *args, **kwargs):
            text = original_read(path, *args, **kwargs)
            if path == dockerfile:
                text += "\nFROM registry.example.org/unreviewed:1\n"
            return text
        with patch.object(Path, "read_text", read), patch("subprocess.check_output", side_effect=[
                json.dumps({"Require": []}), ""]):
            text = script["report"](self.root, "go")
        self.assertIn("| registry.example.org/unreviewed | registry.example.org/unreviewed:1 | licence unknown, review required |", text)
        for licence in ("GPL-3.0 (linuxserver.io image build files)", "LGPL-2.1 (FreeCAD)", "various (Ubuntu base)"):
            self.assertIn(licence, text)
        main = script["main"]
        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp) / "report.md"
            for flags in ([], ["--strict"]):
                with patch.dict(main.__globals__, report=lambda *args: text):
                    with patch.object(sys, "argv", ["license-report", "--output", str(output), *flags]):
                        if flags:
                            with self.assertRaises(SystemExit) as failure:
                                main()
                            self.assertEqual(failure.exception.code, 1)
                        else:
                            main()
                self.assertEqual(output.read_text(), text)

    def test_license_detection_fails_closed(self):
        detect = self.script()["detect"]
        mit = "Permission is hereby granted, free of charge. Keep this permission notice."
        for text in ("", "All rights reserved", "custom proprietary terms", "GPL-3.0", "AGPL-3.0", "SSPL-1.0",
                     "GNU GENERAL PUBLIC LICENSE", "GNU AFFERO GENERAL PUBLIC LICENSE", "Server Side Public License",
                     mit + " GPL-2.0"):
            with self.subTest(text=text), self.assertRaises(ValueError):
                detect(text)
        self.assertEqual(detect(mit), ["MIT"])
        self.assertEqual(detect(mit + " Apache License Version 2.0"), ["Apache-2.0", "MIT"])
        self.assertEqual(detect("Redistribution and use in source and binary forms. THIS SOFTWARE IS PROVIDED AS IS."), ["BSD-2-Clause"])
        self.assertEqual(detect("Redistribution and use in source and binary forms. Neither the name. THIS SOFTWARE IS PROVIDED AS IS."), ["BSD-3-Clause"])
        self.assertEqual(detect("Permission to use, copy, modify, and/or distribute this software."), ["0BSD"])

    def test_license_files_missing_or_unknown(self):
        licenses = self.script()["module_licenses"]
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            with self.assertRaisesRegex(ValueError, "no LICENSE"):
                licenses(root)
            (root / "LICENSE").write_text("unrecognised terms")
            with self.assertRaisesRegex(ValueError, "unknown"):
                licenses(root)
            (root / "LICENSE").write_text("Permission is hereby granted, free of charge. Keep this permission notice.")
            (root / "COPYING").write_text("AGPL-3.0")
            with self.assertRaisesRegex(ValueError, "prohibited"):
                licenses(root)

    def test_required_module_cannot_be_omitted(self):
        report = self.script()["report"]
        required = json.dumps({"Require": [{"Path": "example.org/dependency", "Version": "v1.0.0"}]})
        for module in ({}, {"Error": {"Err": "unavailable"}}, {"Dir": "/missing", "Replace": {"Path": "../local"}}):
            with self.subTest(module=module):
                listed = json.dumps({"Path": "example.org/dependency", "Version": "v1.0.0", **module})
                outputs = [required, listed] if module.get("Replace") else [required, listed, "", listed]
                with patch("subprocess.check_output", side_effect=outputs) as run:
                    with self.assertRaises(ValueError):
                        report(self.root, "go")
                    self.assertEqual(run.call_count, len(outputs))
        with patch("subprocess.check_output", side_effect=[required, ""]):
            with self.assertRaisesRegex(ValueError, "modules missing"):
                report(self.root, "go")

    def test_missing_module_downloaded_once(self):
        report = self.script()["report"]
        module = {"Path": "example.org/dependency", "Version": "v1.0.0"}
        required = json.dumps({"Require": [module]})
        with tempfile.TemporaryDirectory() as temp:
            (Path(temp) / "LICENSE").write_text(
                "Permission is hereby granted, free of charge. Keep this permission notice.")
            cached = json.dumps({**module, "Dir": temp})
            for listed, outputs in ((json.dumps(module), ["", cached]), (cached, [])):
                with self.subTest(cached=not outputs):
                    with patch("subprocess.check_output", side_effect=[required, listed, *outputs]) as run:
                        self.assertIn("| example.org/dependency | v1.0.0 | MIT |", report(self.root, "custom-go"))
                        downloads = [call for call in run.call_args_list
                                     if call.args[0] == ["custom-go", "mod", "download"]]
                        self.assertEqual(len(downloads), int(bool(outputs)))
                        if downloads:
                            self.assertEqual(downloads[0].kwargs["cwd"], self.root / "manager")


if __name__ == "__main__":
    unittest.main()
