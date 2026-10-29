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
                            "--output", str(regenerated)], check=True)
            before, after = report.read_text(), regenerated.read_text()
            self.assertEqual(before, after, "".join(difflib.unified_diff(
                before.splitlines(True), after.splitlines(True), fromfile=str(report), tofile="regenerated")))

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
                with patch("subprocess.check_output", side_effect=[required, listed]):
                    with self.assertRaises(ValueError):
                        report(self.root, "go")
        with patch("subprocess.check_output", side_effect=[required, ""]):
            with self.assertRaisesRegex(ValueError, "modules missing"):
                report(self.root, "go")


if __name__ == "__main__":
    unittest.main()
