---
description: Revise a result, the old version stays
---
Revise the file `$1` like this: $2

Rules:
- The existing file stays unchanged.
- The new file gets the next free version name (`_v2`, `_v3` …): `python3 /etc/opencode/skills/data/scripts/next_name.py <file>`.
- If the file is not in `results/`, the new version still goes to `results/`.
- If the file or the change request is missing, ask once.

End with the new file name and two to three sentences on what changed.
