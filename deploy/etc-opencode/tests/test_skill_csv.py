# SPDX-License-Identifier: MIT

"""CSV detection and preview checks; pandas is optional."""
import importlib.util
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "skills/data/scripts/csv_check.py"


@unittest.skipUnless(importlib.util.find_spec("pandas"), "requires pandas")
class CSVCheckTest(unittest.TestCase):
    def test_preview_and_empty_file(self):
        detect = runpy.run_path(str(SCRIPT))["detect"]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "data.csv"
            path.write_bytes("Month;Total\nJänner;1.234,50\nFebruar;2.000,25\n".encode("cp1252"))
            self.assertEqual(detect(str(path)), {
                "sep": ";", "encoding": "cp1252", "decimal": ",", "thousands": ".",
            })
            result = subprocess.run([sys.executable, "-I", "-B", str(SCRIPT), str(path)],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("2 Zeilen, 2 Spalten", result.stdout)
            self.assertIn("1234.50", result.stdout)
            path.write_bytes(b"")
            self.assertEqual(detect(str(path)), {"sep": ",", "encoding": "utf-8-sig"})


if __name__ == "__main__":
    unittest.main()
