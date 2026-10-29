---
name: plain-language
description: "Rewrite texts in plain language (Einfache Sprache) or Easy Language (Leichte Sprache), following DIN ISO 24495-1, DIN SPEC 33429 and the rules of Netzwerk Leichte Sprache. Load for plain language, easy-to-read, Easy Language, simplify, accessible, Leichte Sprache, Einfache Sprache, verständlich, barrierefrei, vereinfachen, Inklusion."
metadata:
  workspace/version: "1.1.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/tier: "optional"
  workspace/title: "Plain and Easy Language"
  workspace/title-de: "Einfache und Leichte Sprache"
  workspace/description-de: "Texte in Einfache Sprache oder Leichte Sprache übertragen, nach DIN ISO 24495-1, DIN SPEC 33429 und den Regeln des Netzwerks Leichte Sprache."
  workspace/category: "texts"
---
# Einfache Sprache and Leichte Sprache

German plain language. The rules below are for German texts; for English plain language apply ISO 24495-1 in the same spirit.

## Which level
- **Einfache Sprache** (plain language): for everyone who wants to understand fast. Short sentences, familiar words, clear structure. Basis: DIN ISO 24495-1 „Einfache Sprache, Grundsätze und Leitlinien“ (2024).
- **Leichte Sprache** (easy-to-read): for people with learning difficulties and low reading skills. Fixed rules, very short sentences. Basis: DIN SPEC 33429 „Empfehlungen für Deutsche Leichte Sprache“ (2025, free from DIN Media) and the rules of the Netzwerk Leichte Sprache (BMAS guide).
- If the person only says "more understandable" („verständlicher“), use Einfache Sprache.

## Rules Einfache Sprache
- One thought per sentence, about 15 words, most important first.
- Active instead of passive, verbs instead of nominal style („wir prüfen“, not „die Prüfung erfolgt“).
- Avoid jargon or explain it at first use. Spell out abbreviations.
- Address readers directly where it fits.
- Headings that say what it is about; lists for steps.

## Rules Leichte Sprache
- One sentence, one statement. No subordinate clause, no passive, no subjunctive, no genitive („das Haus von Anna“).
- Always the same word for the same thing. No idioms, no irony.
- Split long words with a middle dot or hyphen: „Ausstellungs·raum“ or „Ausstellungs-Raum“. One spelling throughout.
- Numbers as digits. Paraphrase large numbers („viele Menschen“) if the exact number does not matter. Date as „17. Oktober 2026“.
- Explain difficult words, right after them or in a box.
- Every sentence on a new line; left-aligned, no justified text.
- Phrase negations positively where possible.

## Procedure
1. Read the source text from `inbox/`, key statements as a list in `work/kernaussagen.md`.
2. Rewrite, do not rebuild sentence by sentence. Invent nothing; name in the chat what was left out.
3. Deliver a list of the explained words.
4. As Word with skill `docx` (font at least 14 pt, line spacing 1.5) or as PDF via `workspace_md_to_pdf`.
- **New file: never overwrite.** If the person wants an existing file changed, change that same file (AGENTS.md). Name for a new file: `python3 /etc/opencode/skills/data/scripts/next_name.py results/<name>_leichte-sprache.docx`.

## Limits
- Texts in Leichte Sprache should be checked by people from the target group (Prüfgruppe). Without that check the text is called „Entwurf in Leichter Sprache“ and must not carry the seal. Always say so.
- The standards themselves are not available. For details: `workspace_web_search` for "DIN SPEC 33429" or "Netzwerk Leichte Sprache Regeln" and link the source.
