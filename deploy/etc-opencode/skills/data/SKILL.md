---
name: data
description: "Read, clean and analyse CSV and table data (separators, decimal comma, umlauts, duplicates, key figures). Load for CSV, export, clean data, count, group, statistics, CSV, Export, Daten bereinigen, zählen, gruppieren, Statistik."
metadata:
  workspace/version: "1.1.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/title: "Analyse CSV and data"
  workspace/title-de: "CSV und Daten auswerten"
  workspace/description-de: "CSV- und Tabellendaten einlesen, bereinigen und auswerten (Trennzeichen, Dezimalkomma, Umlaute, Dubletten, Kennzahlen)."
---
# Data (CSV, cleaning, analysis)

## Read CSV
`python3 /etc/opencode/skills/data/scripts/csv_check.py inbox/<file>.csv` detects encoding (utf-8 or cp1252), separator and decimal comma and prints the matching `pd.read_csv` arguments plus the first rows. Use these arguments.

Typical for German exports: `sep=";"`, `decimal=","`, `thousands="."`, `encoding="cp1252"`.

## Clean
- Empty rows and columns: `df.dropna(how="all")`, `df.dropna(axis=1, how="all")`.
- Column names: strip surrounding spaces (`df.columns.str.strip()`).
- Duplicates: `df.duplicated(keep=False)`; report count and examples, remove only after asking.
- Dates: `pd.to_datetime(s, dayfirst=True, errors="coerce")`; report the count of unreadable values.
- Text to number (German format): `pd.to_numeric(s.str.replace(".", "").str.replace(",", "."), errors="coerce")`.
- Log every cleaning step with the number of affected rows.

## Analyse
- Key figures with pandas (`groupby`, `pivot_table`, `value_counts`), never by mental arithmetic.
- Save the calculation as a short script in `work/` so it stays traceable.
- State the calculation in one sentence in the chat.

## Output
- **New file: never overwrite.** If the person wants an existing file changed, change that same file (AGENTS.md). Name for a new file: `python3 /etc/opencode/skills/data/scripts/next_name.py results/<name>.<ext>`.
- CSV for German Excel: `df.to_csv(path, sep=";", decimal=",", index=False, encoding="utf-8-sig")`.
- More readable as xlsx: skill `xlsx`.
