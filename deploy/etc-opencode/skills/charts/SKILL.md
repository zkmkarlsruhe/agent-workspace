---
name: charts
description: "Create charts with matplotlib, with readable labels, as PNG and SVG. Load for chart, diagram, graph, bar, line, trend, visualise, Diagramm, Grafik, Chart, Balken, Linie, Verlauf, visualisieren."
metadata:
  workspace/version: "1.2.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/title: "Charts"
  workspace/title-de: "Diagramme"
  workspace/description-de: "Diagramme mit matplotlib erstellen, mit lesbaren Beschriftungen, als PNG und SVG."
---
# Charts

## Helpers
```python
import sys; sys.path.insert(0, "/etc/opencode/skills/charts/scripts")
from workspace_chart import german_axis, save, TITLE_FONT
```
- `german_axis(ax, axis="y", decimals=0)`: numbers with thousands point and decimal comma, only for German charts.
- `save(fig, "results/<name>")`: writes `<name>.png` (200 dpi) and `<name>.svg`, never overwriting (`_v2`, `_v3` …), returns the paths.

## Chart type
- Comparing categories: bars, horizontal (`barh`) for long names.
- Change over time: line.
- Shares: stacked bar; pie only on explicit request and with at most 5 parts.
- No 3D, no shadows.

## Readability
- The title states the message ("Visitors 2025 per month"; German: „Besucherzahlen 2025 je Monat“), axes with unit ("visitors", "euro").
- Set month names and labels yourself in the language of the result; do not rely on the system locale.
- Font size at least 11, size e.g. `figsize=(10, 6)`.
- Values on bars: `ax.bar_label(bars, labels=[f"{v:,.0f}".replace(",", ".") for v in values])` for German; English without `.replace`.
- One colour per series; at most 6 series, else split. Same category, same colour across all charts.
- `fig.tight_layout()` before saving.

## Embed
- In docx: `doc.add_picture("results/<name>.png", width=Cm(16))` (skill `docx`).
- After saving, look at the PNG with `workspace_image_view` and check nothing is cut off.
