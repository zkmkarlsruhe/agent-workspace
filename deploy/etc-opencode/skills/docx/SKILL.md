---
name: docx
description: "Read and create Word documents (docx) with python-docx, e.g. a report, letter or minutes. Load for Word, docx, document, report, letter, Word, docx, Dokument, Bericht, Brief."
metadata:
  workspace/version: "1.2.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/title: "Word documents"
  workspace/title-de: "Word-Dokumente"
  workspace/description-de: "Word-Dokumente (docx) lesen und erstellen mit python-docx, z. B. Bericht, Brief, Protokoll."
---
# Word documents (docx)

## Read
- Text: `Document(path).paragraphs`, with `p.style.name` for headings (`Heading 1` …).
- Tables: `doc.tables`, cells via `table.rows[i].cells[j].text`.
- Fast as Markdown: `pandoc inbox/<file>.docx -t gfm -o work/<file>.md`, then `read`.
- Headers and footers: `doc.sections[0].header.paragraphs`.

## Create
- **New file: never overwrite.** If the person wants an existing file changed, change that same file (AGENTS.md). Name for a new file: `python3 /etc/opencode/skills/data/scripts/next_name.py results/<name>.docx`.
- Styles instead of direct formatting: `doc.add_heading(text, level=1)`, `doc.add_paragraph(text, style="List Bullet")`. This keeps the table of contents usable in Word.
- Heading hierarchy without jumps (1, 2, 3).
- Tables: `doc.add_table(rows, cols, style="Table Grid")`, header row bold.
- Images: `doc.add_picture(path, width=Cm(16))`; create charts first with skill `charts`.

- Convert Markdown with `pandoc work/text.md -o results/<name>.docx` after choosing a free filename.

## Limits
- Word track changes and comments are not supported; say so when asked.
- Conversion to PDF is not available yet (no LibreOffice in the container). The person can save the document as PDF in Word.

## Check
- Proofread the result with `pandoc results/<name>.docx -t plain`: order, headings, tables complete.
