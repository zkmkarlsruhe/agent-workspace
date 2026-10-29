---
description: Researches a question with sources from project files and the web
---
Question: $ARGUMENTS

1. If the question is ambiguous (person, period, purpose, scope), ask once. Write question, purpose and scope to `work/research-brief.md`.
2. Split the question into 2 to 4 sub-questions. Hand each to the subagent `researcher`, with the path to the brief.
3. From the returns write the answer into the chat, no file: short answer, report with numbered citations [1], "Contradictions and uncertainties", "Still open", list of sources (file with page or section; web page with title, URL and "retrieved on" from the tool output). Headings in the person's language.
4. Drop statements without evidence or mark them as your own assessment.
5. Then offer in one short last line: store as PDF (when `workspace_md_to_pdf` is offered), go deeper or done. No `question` tool.
6. On "store as PDF": write the report to `results/<topic>-research_<yyyy-mm-dd>.md` (German: `<thema>-recherche_<jjjj-mm-tt>.md`; never overwrite), call `workspace_md_to_pdf` with that path at once and name the PDF; without the tool, say the PDF export is not available here and keep the .md.
