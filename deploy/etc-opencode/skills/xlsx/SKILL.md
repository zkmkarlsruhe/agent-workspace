---
name: xlsx
description: "Read, analyse, create or revise Excel spreadsheets (xlsx) with pandas and openpyxl. Load for Excel, spreadsheet, workbook, sheet, sums, formulas, Excel, Tabelle, Arbeitsmappe, Blatt, Summen, Formeln."
metadata:
  workspace/version: "1.1.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/title: "Excel spreadsheets"
  workspace/title-de: "Excel-Tabellen"
  workspace/description-de: "Excel-Tabellen (xlsx) lesen, auswerten, neu anlegen oder überarbeiten mit pandas und openpyxl."
---
# Excel spreadsheets (xlsx)

## Look first
`python3 /etc/opencode/skills/xlsx/scripts/xlsx_overview.py inbox/<file>.xlsx` shows every sheet with size, header row, formula count, merged cells and formulas without a stored value.

## Read
- All sheets: `pd.read_excel(path, sheet_name=None)` returns a dict sheet name -> DataFrame.
- The header is not always in row 1: look with `header=None` first, then set `header=<n>`.
- Merged cells arrive as empty cells; fill values down with `ffill()` if they belong to a group.
- Formulas and values separately: openpyxl `load_workbook(path)` returns formulas, `load_workbook(path, data_only=True)` the values Excel last saved (`None` for files never opened in Excel).

## Write
- **New file: never overwrite.** If the person wants an existing file changed, change that same file (AGENTS.md). Name for a new file: `python3 /etc/opencode/skills/data/scripts/next_name.py results/<name>.xlsx`.
- New file from DataFrames: `pd.ExcelWriter(path, engine="xlsxwriter")` or openpyxl.
- Revise an existing file: load with openpyxl (without `data_only`, else the formulas are lost), change, save under the same name (AGENTS.md: editing changes the same file).
- Font: new sheets in Open Sans, the Workspace base font: xlsxwriter `workbook.formats[0].set_font_name("Open Sans")` right after creating, openpyxl `Font(name="Open Sans")` for cell styles you set.
- **No recalculation in the container** (no LibreOffice): formulas you write have no stored value until someone opens the file in Excel. So also write important results as values, e.g. a sum row with formula and next to it a column "Value (calculated)" („Wert (berechnet)“ in German) with the result computed in Python. Tell the person Excel calculates the formulas on opening.

## Layout
- Header row bold, frozen (`ws.freeze_panes = "A2"`), autofilter (`ws.auto_filter.ref = ws.dimensions`).
- Column width by longest content, at most 60.
- Number formats: thousands `#,##0`, decimals `#,##0.00`, euro `#,##0.00 "€"`, date `DD.MM.YYYY` for German results. Excel shows them with the reader's locale separators.
- One sheet per topic, short sheet names in the language of the result (max. 31 characters).

## Check
- Recompute sums and counts with pandas against the input data; state the calculation in one sentence.
- After saving, reopen the file with openpyxl and read samples.
- For numbers in reports: subagent `reviewer`.
