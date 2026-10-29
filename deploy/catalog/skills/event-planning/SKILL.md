---
name: event-planning
description: "Plan an event with a schedule worked back from the date as Excel, a checklist, a run sheet for the day and a budget overview. Load for event, opening, reading, workshop, conference, schedule, checklist, run sheet, Veranstaltung, Event, Eröffnung, Lesung, Workshop, Tagung, Zeitplan, Checkliste, Ablaufplan."
metadata:
  workspace/version: "1.1.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/tier: "optional"
  workspace/title: "Plan an event"
  workspace/title-de: "Veranstaltung planen"
  workspace/description-de: "Veranstaltung planen mit Zeitplan rückwärts vom Termin als Excel, Checkliste, Ablaufplan für den Tag und Budgetübersicht."
  workspace/category: "organisation"
---
# Plan an event

## Clarify
Title, date, time, venue, expected guests, format (opening, reading, workshop, conference), budget. Ask once, precisely, for what is missing. Invent nothing: no names, prices or commitments.

## Schedule worked back from the date
1. Get the target name: `python3 /etc/opencode/skills/data/scripts/next_name.py results/zeitplan-<title>.xlsx`
2. Create: `python3 /opt/workspace/catalog/skills/event-planning/scripts/schedule.py <DD.MM.YYYY> <target> "<Title>"`
   - 22 standard tasks from 12 weeks before to 2 weeks after; columns (German) Fällig am, Wochentag, Bereich, Aufgabe, Verantwortlich, Status.
   - Due dates on a weekend move to the Friday before. The script knows no public holidays; name holidays nearby if the person asks (`workspace_web_search`).
   - If the script reports "Already past", tell the person which tasks are due now.
3. Adjust with openpyxl (skill `xlsx`): delete tasks that do not fit, add own ones; fill in people responsible only if the person names them. Translate the sheet if the person works in English.

## Run sheet for the day
Table with time, duration, item, place, who, technology, note. Plan set-up and tear-down. As a second sheet "Ablauf" (English: "Run sheet") in the same file, or as Word with skill `docx`.

## Budget
- Sheet "Budget": item, supplier, quantity, unit price, total, status (quote, ordered, paid), receipt.
- Only amounts from quotes in `inbox/` or from the person. Price missing: leave empty and name it as open.
- Compute sums with pandas and write them as values, formula next to them (no LibreOffice to recalculate).

## Checklist (by format)
- Clarify room, seating, escape routes and permitted capacity with the house.
- Accessibility: step-free access, hearing loop, seats, texts in Einfache Sprache (skill `plain-language`).
- Rights: music (GEMA), photos and filming of guests (notice at the entrance), image rights for the announcement.
- Communication: press release (skill `press-release`), invitation (skill `letter-email`).
- This list does not replace the rules of the house, security or administration. Those are binding; say so.

## Output
Schedule, run sheet and budget in one workbook in `results/`, never overwrite. In the chat: the next three due tasks with dates.
