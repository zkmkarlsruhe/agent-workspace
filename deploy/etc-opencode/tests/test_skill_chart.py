# SPDX-License-Identifier: MIT

"""Chart formatting and paired exports work with either distribution's helper."""
import importlib.util
from pathlib import Path
import runpy
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parents[1] / "skills/charts/scripts"


@unittest.skipUnless(importlib.util.find_spec("matplotlib"), "requires matplotlib")
class ChartTest(unittest.TestCase):
    def test_save_and_existing_target(self):
        # The public distribution uses a neutral module name.
        script, = SCRIPTS.glob("*_chart.py")
        chart = runpy.run_path(str(script))
        import matplotlib.pyplot as plt

        figure, axes = plt.subplots()
        self.addCleanup(plt.close, figure)
        axes.bar(["Jan", "Feb"], [1200, 1500])
        chart["german_axis"](axes, decimals=1)
        self.assertEqual(axes.yaxis.get_major_formatter()(1234.5), "1.234,5")
        self.assertEqual(chart["number_de"](-1000), "-1.000")
        with tempfile.TemporaryDirectory() as directory:
            stem = Path(directory) / "results" / "totals"
            paths = [Path(path) for path in chart["save"](figure, str(stem))]
            self.assertEqual(paths, [stem.with_suffix(".png"), stem.with_suffix(".svg")])
            self.assertTrue(paths[0].read_bytes().startswith(b"\x89PNG\r\n\x1a\n"))
            self.assertIn("<svg", paths[1].read_text())
            paths[0].unlink()
            original = paths[1].read_bytes()
            versions = [Path(path) for path in chart["save"](figure, str(stem))]
            self.assertEqual(versions, [stem.with_name("totals_v2.png"), stem.with_name("totals_v2.svg")])
            self.assertTrue(all(path.stat().st_size > 0 for path in versions))
            self.assertEqual(paths[1].read_bytes(), original)
            self.assertFalse(paths[0].exists())


if __name__ == "__main__":
    unittest.main()
