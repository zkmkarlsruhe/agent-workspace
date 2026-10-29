---
name: press-release
description: "Write a press release or announcement from source material, with headline, lead, the five Ws, quote and info box, using only verified facts. Load for press release, announcement, media release, newsletter text, Pressetext, Pressemitteilung, Ankündigung, Medieninformation, Newsletter-Text."
metadata:
  workspace/version: "1.1.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/tier: "optional"
  workspace/title: "Press release"
  workspace/title-de: "Pressetext"
  workspace/description-de: "Pressemitteilung oder Ankündigung aus Unterlagen schreiben, mit Überschrift, Lead, W-Fragen, Zitat und Infokasten, nur mit belegten Fakten."
  workspace/category: "texts"
---
# Press release

## Collect first
- Read the material in `inbox/` (concept, list of works, biographies, earlier texts) with skills `docx`, `pdf`.
- Fact list in `work/fakten.md`: title, artists, venue (room, building), run, opening with time, admission, funders, contact. Every line with its source (file, page).
- A required fact (date, venue, title) missing: ask once, precisely. Never guess; never add from the web without saying so.
- Quotes only verbatim from the material or from the person. No invented quote, not even as a placeholder with a real name; use `[Zitat N. N., bitte einholen]` (English: `[quote N. N., to be obtained]`).

## Structure
1. Kicker (optional): series or occasion.
2. Headline: at most 10 words, says what happens.
3. Lead (2 to 3 sentences): who, what, when, where. After it an editor knows everything important.
4. Body: why and how, from important to supplementary. Paragraphs of 3 to 5 sentences.
5. Quote: one voice, one thought.
6. Info box: title, run, venue, opening, admission, link, press contact.
7. Short sentence about the house only if the person supplies a boilerplate.

## Style
- Active voice, present tense, concrete verbs. No superlatives without evidence ("largest", "first" only with a source).
- Full names at first mention, titles of works in italics. German texts: dates as `17. Oktober 2026`, time `19 Uhr`, quotes „…“.
- Write the house name as in the material, e.g. "Workspace | Example Museum".
- Length: about 2,500 to 3,500 characters unless asked otherwise. Count characters with Python and state the count.
- Other language version: skill `translate`.

## Output
- Show it in the chat first. File only on request: Word with skill `docx`, or PDF via `results/<name>.md` and `workspace_md_to_pdf`.
- **New file: never overwrite.** If the person wants an existing file changed, change that same file (AGENTS.md). Name for a new file: `python3 /etc/opencode/skills/data/scripts/next_name.py results/pressetext-<topic>.docx`.

## Check
- Check every info-box item against `work/fakten.md` and list open points under "Please check".
- Approval by the communications department is part of it; say so for public texts.
