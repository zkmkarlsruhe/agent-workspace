# SPDX-License-Identifier: MIT

"""Live acceptance test. Run only in a fresh, dedicated prototype document."""

import json
from probe import execute

CODE = '''
import json, math, workspace_cad
import FreeCAD as App
import FreeCADGui as Gui

initial = workspace_cad.create_plate()
doc = App.ActiveDocument
assert doc.Name == "MountingPlate"
shape = doc.Result.Shape
assert len(shape.Solids) == 1 and shape.isValid()
assert abs(shape.Volume - (60*40*4 - 2*math.pi*2.5**2*4)) < 1e-5
old_revision = workspace_cad.revision(doc)

# Simulate an external edit through the native API, not through our recipe.
# Actual browser input is a separate acceptance check.
doc.LeftHole.Radius = 3
doc.LeftHole.Label = "Manual hole diameter 6 mm"
doc.recompute()
assert workspace_cad.revision(doc) != old_revision
try:
    workspace_cad.set_hole_diameter(doc.Name, old_revision, "RightHole", 7)
except ValueError as exc:
    assert "conflict" in str(exc)
else:
    raise AssertionError("Stale revision was accepted")
assert doc.RightHole.Radius.Value == 2.5

workspace_cad.set_hole_diameter(doc.Name, workspace_cad.revision(doc), "RightHole", 7)
assert doc.LeftHole.Radius.Value == 3
assert doc.RightHole.Radius.Value == 3.5
assert doc.LeftHole.Label == "Manual hole diameter 6 mm"
saved = workspace_cad.save_export(doc.Name, workspace_cad.revision(doc))
assert saved["mesh_solid"] and saved["facets"] > 0
assert all(abs(a-b) < 0.01 for a,b in zip(saved["size_mm"], (60,40,4)))
filename = doc.FileName
before_close = workspace_cad.revision(doc)
App.closeDocument(doc.Name)
reopened = App.openDocument(filename)
assert reopened.LeftHole.Radius.Value == 3
assert reopened.RightHole.Radius.Value == 3.5
assert reopened.Result.Shape.isValid()
assert workspace_cad.revision(reopened) != before_close
reopened.RightHole.Radius = 4
reopened.recompute()
assert reopened.Result.Shape.isValid()
reopened.RightHole.Radius = 3.5
reopened.recompute()
reopened.save()
Gui.activeDocument().activeView().viewAxonometric()
Gui.activeDocument().activeView().fitAll()
print(json.dumps({"checks": ["solid_dimensions_volume", "external_edit_observed",
    "stale_write_rejected", "external_change_preserved", "stl_reimport",
    "native_file_reopened", "parameters_editable_after_reopen"],
    "export": saved, "state": workspace_cad.state()}))
'''

if __name__ == "__main__":
    output = execute(CODE)
    # CAD exporters may write informational lines before the test's JSON result.
    result = json.loads(output.strip().splitlines()[-1])
    print(json.dumps(result, indent=2))
