# Shared FreeCAD session, contract version 1

The browser and MCP operate on the same visible FreeCAD process. Work in the
current chat project. Start with `freecad_open`; poll when status is `starting`.
Give the returned `browser_url` to the person for a new tab. Keep `session_id`.
A project switch saves and closes the previous application's session.

1. Read `freecad_state`. Inspect document names, revisions, selection, objects,
   dimensions, shape validity and any `autosave_error`.
2. Ask for missing functional dimensions. Lengths use millimetres. Do not invent
   a fit, material allowance or minimum wall thickness.
3. Use `freecad_document` to create a named document or open an existing project
   FCStd file. New names use letters, digits and underscores, beginning with a letter.
4. Use `freecad_execute` for one small change. Supply the exact document name and
   current revision from state, plus a unique `request_id`. `App`, `Gui` and `doc`
   are available in Python. Use native editable features, not an imported mesh
   when a parametric shape is possible. Preserve unrelated manual changes.
5. Read every output page with `freecad_output` while `has_more` is true. Inspect
   stderr and errors. Transport success does not prove a successful operation.
6. Read state again. Verify expected dimensions, valid shape and solid count.
   Use `freecad_view`, then `image_view` on its project-relative PNG path to
   inspect the visible result. Do not claim unseen visual details.
7. `freecad_save` saves native documents. `freecad_export` exports one explicitly
   selected valid solid to STEP or STL. Report returned project-relative paths.
   `freecad_close` saves and stops. Files remain in the project's `freecad` folder.

A new box, after reading the current document and revision:

```python
box = doc.addObject("Part::Box", "Base")
box.Length = 60
box.Width = 40
box.Height = 4
Gui.activeDocument().activeView().viewAxonometric()
Gui.activeDocument().activeView().fitAll()
```

Those numbers illustrate API use; they are not defaults for a requested design.
To change a feature, obtain its actual name from state, then change its parameter:
`doc.getObject("Base").Height = 6`. Do not rebuild the whole document for one edit.

Each execution checks revision before starting, creates an FCStd recovery copy,
uses a transaction, recomputes, checks object error states, then saves. GUI input
is disabled during execution. Pending GUI edits must finish first. A conflict
requires a fresh read and a new decision; never reuse a stale revision.

Never repeat a write after a timeout or connection loss. Inspect current state
and the existing job output. Reusing the same `request_id` returns its recorded
response; changing arguments under an existing ID is rejected. This journal is
session-local. Old session IDs and revisions are invalid after a restart.

Python runs with the container user's permissions, not inside a Python sandbox.
Do not close documents, process events recursively, spawn background work, write
outside the current project or change application/system configuration. These
checks do not make arbitrary scripts atomic or recover external side effects.
Native changes are autosaved every 15 seconds when no GUI transaction is open.
A crash can lose changes since the last successful save. Check `autosave_error`.
