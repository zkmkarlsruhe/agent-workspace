# SPDX-License-Identifier: MIT

"""Free result names must not overwrite existing versions."""
from pathlib import Path
import runpy
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "skills/data/scripts/next_name.py"


class NextNameTest(unittest.TestCase):
    def test_free_and_existing_names(self):
        next_name = runpy.run_path(str(SCRIPT))["next_name"]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "report.xlsx"
            self.assertEqual(next_name(str(path)), path)
            self.assertFalse(path.exists())
            path.write_bytes(b"original")
            version = path.with_name("report_v2.xlsx")
            version.write_bytes(b"second")
            for requested in (path, version):
                self.assertEqual(next_name(str(requested)), path.with_name("report_v3.xlsx"))
            self.assertEqual(path.read_bytes(), b"original")
            self.assertEqual(version.read_bytes(), b"second")


if __name__ == "__main__":
    unittest.main()
