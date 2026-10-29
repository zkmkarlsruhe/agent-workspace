---
name: pdf
description: "Read PDF files, extract text and tables, merge or split pages with pypdf and pdftotext. Load for PDF, pages, scan, merge, split, PDF, Seiten, Scan, zusammenfügen, teilen."
metadata:
  workspace/version: "1.1.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/title: "Read and edit PDFs"
  workspace/title-de: "PDF lesen und bearbeiten"
  workspace/description-de: "PDF-Dateien lesen, Text und Tabellen herausziehen, Seiten zusammenfügen oder teilen mit pypdf und pdftotext."
---
# PDF

## Read
- Small PDFs (up to about 20 MiB): read directly with the `read` tool.
- Large PDFs or specific pages: `pdftotext -layout -f <from> -l <to> inbox/<file>.pdf work/<file>.txt`, then `read` or `grep` the text file.
- Page count: `pdfinfo inbox/<file>.pdf`.
- Always keep page references: cite as "<file>.pdf, p. 12" (German: „<datei>.pdf, S. 12“). With pypdf: `PdfReader(path).pages[i].extract_text()` returns page i+1.

## Scanned PDFs
- If `pdftotext` returns almost no text, it is a scan. OCR: `pdftoppm -r 300 -png <file>.pdf work/page`, then `tesseract work/page-1.png work/page-1 -l deu+eng`.
- Mark the result as "read by OCR, errors possible".

## Tables from PDF
- `pdftotext -layout` keeps columns as blocks of spaces; split them in Python with rules for the column borders and save as xlsx (skill `xlsx`).
- Check 10 samples against the PDF and report the result.

## Edit
- **New file: never overwrite.** If the person wants an existing file changed, change that same file (AGENTS.md). Name for a new file: `python3 /etc/opencode/skills/data/scripts/next_name.py results/<name>.pdf`.
- Merge: `qpdf --empty --pages a.pdf b.pdf -- results/<name>.pdf` (or pypdf `PdfWriter.append`).
- Split: `qpdf inbox/<file>.pdf --pages . 1-5 -- results/<name>-p1-5.pdf`.
- Rotate: `qpdf --rotate=+90:1 …`.

## Limits
- New PDFs from text: write Markdown to `results/<name>.md`, then, when the tool `workspace_md_to_pdf` is offered, call it with that path (A4, Workspace template); without it, tell the person the PDF export is not available here and keep the .md. pandoc has no PDF export in the container.
