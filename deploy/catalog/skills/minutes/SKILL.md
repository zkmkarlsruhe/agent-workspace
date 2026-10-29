---
name: minutes
description: "Write meeting minutes from notes or a transcript, with participants, decisions and a task list (who, what, by when). Load for minutes, meeting, meeting notes, transcript, action items, Protokoll, Besprechung, Sitzung, Meeting, Mitschrift, Ergebnisprotokoll, Aufgaben."
metadata:
  workspace/version: "1.1.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/tier: "optional"
  workspace/title: "Meeting minutes"
  workspace/title-de: "Protokoll"
  workspace/description-de: "Besprechungsprotokoll aus Notizen, Mitschrift oder Transkript erstellen, mit Teilnehmenden, Beschlüssen und Aufgabenliste (wer, was, bis wann)."
  workspace/category: "organisation"
---
# Meeting minutes

## Clarify the kind
- Results minutes (default): decisions, tasks, open points only.
- Verbatim-style minutes only on request: who said what, in short form.
- If the person has a template in `inbox/`, use its structure.

## Read
- Notes, transcript or chat log from `inbox/`. docx with skill `docx`, PDF with skill `pdf`.
- Add nothing that is not in the source. Mark unclear passages as „[unklar: …]“ (English: "[unclear: …]") and collect them at the end.

## Structure (German template; use English headings when the person works in English)
```markdown
# Protokoll: <Titel der Besprechung>
Datum: <TT.MM.JJJJ>, <Uhrzeit von bis> | Ort: <Raum oder online>
Teilnehmende: <Namen> | Entschuldigt: <Namen> | Protokoll: <Name>

## Tagesordnung
1. ...

## Ergebnisse
### TOP 1 <Thema>
- Beschluss: ...
- Diskussion (kurz, nur wenn nötig): ...

## Aufgaben
| Nr. | Aufgabe | Verantwortlich | Bis | Status |

## Offene Punkte und nächster Termin
```
- Tasks always with person responsible and date. One missing: enter „offen“ ("open") and name it in the chat.
- Decisions as complete sentences in the indicative: „Die Eröffnung findet am 12. März statt.“
- Write names as they appear in the source. No judgement of people, no quotes about third parties that the result does not need.

## Output
- In the chat first. File on request: Word with skill `docx` (from Markdown via `pandoc`), PDF via `results/<name>.md` and `workspace_md_to_pdf`.
- Task list also as Excel if the person wants to keep working on it: skill `xlsx`, columns as above, dates as `DD.MM.YYYY` in German files.
- **New file: never overwrite.** If the person wants an existing file changed, change that same file (AGENTS.md). Name for a new file: `python3 /etc/opencode/skills/data/scripts/next_name.py results/protokoll-<topic>_<YYYY-MM-DD>.docx`.

## Limits
- The helper cannot listen to audio or video files. The person needs a text version (transcript).
- The minutes are a draft until the participants approve them; say so at the end.
