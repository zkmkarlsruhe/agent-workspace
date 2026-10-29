---
name: translate
description: "Translate texts from German to English and from English to German, with fixed Workspace terms, a glossary and a checklist, also for Word files. Load for translation, English version, German version, glossary, übersetzen, Übersetzung, translate, englische Fassung, deutsche Fassung, Glossar."
metadata:
  workspace/version: "1.1.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/tier: "optional"
  workspace/title: "Translate DE/EN"
  workspace/title-de: "Übersetzen DE/EN"
  workspace/description-de: "Texte Deutsch-Englisch und Englisch-Deutsch übersetzen, mit festen Workspace-Begriffen, Glossar und Prüfliste, auch für Word-Dateien."
  workspace/category: "texts"
---
# Translate German and English

## Prepare
- Clarify purpose and readers if unclear: wall text, press release, contract, mail. Contracts and legal texts: translate, but mark clearly as a "non-binding working translation" (German: „nicht rechtsverbindliche Arbeitsübersetzung“).
- A glossary in `inbox/` (e.g. `glossar.xlsx`, columns German, English) wins over everything else. Otherwise the list below.
- British or American English: as in existing English texts in `inbox/`, else clarify with the first question. Stay consistent across the text; proper names such as "Center" in the house name stay unchanged.

## Fixed terms (without glossary)
| German | English |
|---|---|
| Workspace \| Example Museum | Workspace \| Example Museum |
| Ausstellung | exhibition |
| Eröffnung | opening |
| Laufzeit | duration / on view (date) |
| Werkliste | list of works |
| Leihgabe / Leihgeber | loan / lender |
| Kurator, Kuratorin | curator |
| Medienkunst | media art |
| Vermittlung | education and outreach |
| Eintritt frei | free admission |

Proper names, titles of works and series stay in the original unless the material has an official English title. Never translate a title yourself and present it as official; give the original plus the translation in square brackets.

## Translate
- Paragraph by paragraph, meaning before wording. Leave nothing out, add nothing.
- Numbers and dates in the target convention: `17. Oktober 2026` becomes `17 October 2026`, `1.234,50 €` becomes `€1,234.50`, and back.
- German gender forms (colon, asterisk) resolve neutrally: „Künstler:innen“ becomes "artists". English to German: the form used in the material or the one the person asks for.
- Mark uncertain passages with `[?]` and explain them at the end.

## Word files
- Read with skill `docx`; short texts: replace paragraph by paragraph and keep the styles; long texts: via `pandoc` to Markdown and back.
- **New file: never overwrite.** If the person wants an existing file changed, change that same file (AGENTS.md). Name for a new file: `python3 /etc/opencode/skills/data/scripts/next_name.py results/<name>_en.docx`.

## Check
- Finally: every name, number, date and link of the original is in the translation. Compare the digit sequences of both texts with Python and name differences.
- Public texts go to a person confident in the target language before publication; say so.
