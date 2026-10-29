# SPDX-License-Identifier: MIT

"""Chart helpers: German number format on axes, saving as PNG and SVG without overwriting.

import sys; sys.path.insert(0, "/etc/opencode/skills/charts/scripts")
from workspace_chart import german_axis, save, TITLE_FONT
ax.set_title("…", fontdict=TITLE_FONT)  # title in the house font
"""
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
from matplotlib.ticker import FuncFormatter  # noqa: E402

# Use installed free fonts; keep text as text in SVG.
matplotlib.rcParams["font.family"] = "sans-serif"
matplotlib.rcParams["font.sans-serif"] = ["Open Sans", "DejaVu Sans"]
matplotlib.rcParams["axes.titlesize"] = "large"
matplotlib.rcParams["svg.fonttype"] = "none"
TITLE_FONT = {"family": ["Open Sans", "DejaVu Sans"], "weight": "medium"}


def number_de(value: float, decimals: int = 0) -> str:
    """1234567.5 -> '1.234.567,5' (with decimals=1)."""
    return f"{value:,.{decimals}f}".replace(",", "X").replace(".", ",").replace("X", ".")


def german_axis(ax, axis: str = "y", decimals: int = 0) -> None:
    fmt = FuncFormatter(lambda v, _pos: number_de(v, decimals))
    (ax.yaxis if axis == "y" else ax.xaxis).set_major_formatter(fmt)


def save(fig, stem: str) -> list[str]:
    """Writes <stem>.png and <stem>.svg; if either name is taken, both get _v2, _v3 ..."""
    base = Path(stem)
    n, candidate = 1, base
    while candidate.with_suffix(".png").exists() or candidate.with_suffix(".svg").exists():
        n += 1
        candidate = base.with_name(f"{base.name}_v{n}")
    candidate.parent.mkdir(parents=True, exist_ok=True)
    paths = [str(candidate.with_suffix(".png")), str(candidate.with_suffix(".svg"))]
    fig.savefig(paths[0], dpi=200, bbox_inches="tight")
    fig.savefig(paths[1], bbox_inches="tight")
    return paths
