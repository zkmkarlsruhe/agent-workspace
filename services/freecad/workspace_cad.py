# SPDX-License-Identifier: MIT

"""Project persistence and revision-checked operations in the live GUI thread."""

import json
import math
import os
from pathlib import Path
import uuid
import re

import FreeCAD as App
import FreeCADGui as Gui

SESSION = os.environ.get("WORKSPACE_FREECAD_SESSION", str(uuid.uuid4()))
_revisions = {}
_autosave_error = None


class Observer:
    def slotCreatedDocument(self, doc):
        _revisions[doc.Name] = [str(uuid.uuid4()), 0]

    def slotDeletedDocument(self, doc):
        _revisions.pop(doc.Name, None)

    def changed(self, doc):
        _revisions.setdefault(doc.Name, [str(uuid.uuid4()), 0])[1] += 1

    def slotCreatedObject(self, obj):
        self.changed(obj.Document)

    def slotDeletedObject(self, obj):
        self.changed(obj.Document)

    def slotChangedObject(self, obj, prop):
        self.changed(obj.Document)

    def slotUndoDocument(self, doc):
        self.changed(doc)

    def slotRedoDocument(self, doc):
        self.changed(doc)


_observer = Observer()
App.addDocumentObserver(_observer)


def revision(doc):
    identity, count = _revisions.setdefault(doc.Name, [str(uuid.uuid4()), 0])
    return f"{SESSION}:{identity}:{count}"


def state():
    documents = []
    for doc in App.listDocuments().values():
        objects = []
        for obj in doc.Objects:
            item = {"name": obj.Name, "label": obj.Label, "type": obj.TypeId,
                    "state": list(obj.State)}
            for prop in ("Length", "Width", "Height", "Radius"):
                if prop in obj.PropertiesList and hasattr(getattr(obj, prop), "Value"):
                    item[prop] = {"value": getattr(obj, prop).Value, "unit": "mm"}
            if hasattr(obj, "Shape") and not obj.Shape.isNull():
                shape = obj.Shape
                bounds = shape.BoundBox
                item["geometry"] = {"valid": shape.isValid(), "solids": len(shape.Solids),
                                    "volume_mm3": shape.Volume,
                                    "size_mm": [bounds.XLength, bounds.YLength, bounds.ZLength]}
            objects.append(item)
        documents.append({"name": doc.Name, "label": doc.Label, "revision": revision(doc),
                          "file": doc.FileName, "objects": objects})
    return {"session": SESSION, "pid": os.getpid(), "gui": App.GuiUp, "autosave_error": _autosave_error,
            "freecad_version": App.Version(),
            "active_document": App.ActiveDocument.Name if App.ActiveDocument else None,
            "selection": [{"document": s.DocumentName, "object": s.ObjectName,
                           "subelements": list(s.SubElementNames)} for s in Gui.Selection.getSelectionEx()],
            "documents": documents}


def _document(name, expected_revision):
    doc = App.getDocument(name)
    if not doc or revision(doc) != expected_revision:
        raise ValueError("Document conflict: read current state before changing it")
    if doc.HasPendingTransaction or Gui.getDocument(name).getInEdit():
        raise ValueError("Document is being edited; finish the current edit first")
    return doc


def _solid(obj):
    if obj.Shape.isNull() or not obj.Shape.isValid() or len(obj.Shape.Solids) != 1:
        raise ValueError("Expected one nonempty valid solid")
    if "Invalid" in obj.State:
        raise ValueError("Object reports an invalid state")


def create_plate():
    """Fixed acceptance fixture: 60 x 40 x 4 mm, two 5 mm holes 40 mm apart."""
    if "MountingPlate" in App.listDocuments():
        raise ValueError("MountingPlate already exists; inspect it instead of repeating creation")
    doc = App.newDocument("MountingPlate")
    doc.openTransaction("Create acceptance plate")
    try:
        base = doc.addObject("Part::Box", "Plate")
        base.Length, base.Width, base.Height = 60, 40, 4
        base.Placement.Base = App.Vector(-30, -20, 0)
        cutters = []
        for name, x in (("LeftHole", -20), ("RightHole", 20)):
            hole = doc.addObject("Part::Cylinder", name)
            hole.Radius, hole.Height = 2.5, 4
            hole.Placement.Base = App.Vector(x, 0, 0)
            cutters.append(hole)
        left = doc.addObject("Part::Cut", "FirstCut")
        left.Base, left.Tool = base, cutters[0]
        final = doc.addObject("Part::Cut", "Result")
        final.Base, final.Tool = left, cutters[1]
        doc.recompute()
        _solid(final)
        for obj in (base, left, *cutters):
            obj.ViewObject.Visibility = False
        final.ViewObject.Visibility = True
        doc.commitTransaction()
    except Exception:
        doc.abortTransaction()
        raise
    Gui.activeDocument().activeView().viewAxonometric()
    Gui.activeDocument().activeView().fitAll()
    return state()


def set_hole_diameter(document, expected_revision, hole_name, diameter_mm):
    if hole_name not in ("LeftHole", "RightHole"):
        raise ValueError("Choose LeftHole or RightHole from the acceptance fixture")
    if isinstance(diameter_mm, bool) or not isinstance(diameter_mm, (int, float)):
        raise ValueError("Diameter must be a number in millimetres")
    if not math.isfinite(diameter_mm) or not 0 < diameter_mm < 20:
        raise ValueError("Fixture diameter must be positive and below 20 mm")
    doc = _document(document, expected_revision)
    hole, final = doc.getObject(hole_name), doc.getObject("Result")
    if not hole or hole.TypeId != "Part::Cylinder" or not final:
        raise ValueError("Acceptance fixture has changed; inspect the document")
    doc.openTransaction("Change hole diameter")
    try:
        hole.Radius = diameter_mm / 2
        doc.recompute()
        _solid(final)
        doc.commitTransaction()
    except Exception:
        doc.abortTransaction()
        raise
    return state()


def save_export(document, expected_revision):
    """Save a new immutable result directory; never overwrite another export."""
    import Mesh
    import MeshPart
    import Part

    doc = _document(document, expected_revision)
    final = doc.getObject("Result")
    if not final:
        raise ValueError("Acceptance fixture Result is missing")
    _solid(final)
    output = Path("/projects") / ("export-" + str(uuid.uuid4()))
    output.mkdir()
    doc.saveAs(str(output / "MountingPlate.FCStd"))
    Part.export([final], str(output / "MountingPlate.step"))
    mesh = MeshPart.meshFromShape(Shape=final.Shape, LinearDeflection=0.05,
                                  AngularDeflection=0.1, Relative=False)
    mesh.write(str(output / "MountingPlate.stl"))
    reloaded = Mesh.Mesh(str(output / "MountingPlate.stl"))
    if not reloaded.CountFacets or not reloaded.isSolid() or reloaded.hasNonManifolds():
        raise ValueError("Exported STL failed closed-mesh validation")
    result = {"directory": str(output), "revision": revision(doc), "unit": "mm",
              "linear_deflection_mm": 0.05, "angular_deflection_rad": 0.1,
              "facets": reloaded.CountFacets, "mesh_solid": reloaded.isSolid(),
              "size_mm": [reloaded.BoundBox.XLength, reloaded.BoundBox.YLength, reloaded.BoundBox.ZLength],
              "physical_print_checked": False}
    (output / "validation.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


def project_root():
    root = Path(os.environ["WORKSPACE_FREECAD_PROJECT"])
    if not root.is_dir() or root.resolve() != root:
        raise ValueError("Project directory missing or redirected")
    return root


def storage():
    root = project_root()
    dest = root / "freecad"
    dest.mkdir(exist_ok=True)
    if dest.resolve() != dest:
        raise ValueError("FreeCAD project directory is redirected")
    return dest


def document(action, name):
    if action == "new":
        if not re.fullmatch(r"[A-Za-z][A-Za-z0-9_]{0,63}", name) or name in App.listDocuments():
            raise ValueError("Use a new document name: letters, digits, underscore")
        App.newDocument(name)
    elif action == "open":
        root = project_root()
        filename = (root / name).resolve()
        if not filename.is_relative_to(root) or filename.suffix.lower() != ".fcstd" or not filename.is_file():
            raise ValueError("Choose an existing FCStd file inside this project")
        existing = next((d for d in App.listDocuments().values() if d.FileName == str(filename)), None)
        if existing:
            App.setActiveDocument(existing.Name)
        else:
            App.openDocument(str(filename))
    else:
        raise ValueError("Unknown document action")
    save_all()
    return state()


def save_all():
    root, dest = project_root(), storage()
    documents = list(App.listDocuments().values())
    active = App.ActiveDocument
    active_file = None
    if any(d.HasPendingTransaction or Gui.getDocument(d.Name).getInEdit() for d in documents):
        raise ValueError("Finish the active edit before saving or closing")
    paths = []
    for doc in documents:
        filename = Path(doc.FileName) if doc.FileName else None
        if not filename or not filename.resolve().is_relative_to(root):
            filename = dest / (doc.Name + "-" + uuid.uuid4().hex[:8] + ".FCStd")
            doc.saveAs(str(filename))
        elif Gui.getDocument(doc.Name).Modified:
            doc.save()
        paths.append(str(filename.relative_to(root)))
        if doc == active:
            active_file = paths[-1]
    tmp = dest / ("session-" + uuid.uuid4().hex + ".json")
    tmp.write_text(json.dumps({"files": paths, "active_file": active_file}) + "\n")
    os.replace(tmp, dest / "session.json")
    return {"saved": True, "files": paths, "unit": "mm"}


def recover_all():
    """Preserve current geometry even when an edit prevents an ordinary save."""
    import time
    dest = storage()
    paths = []
    for doc in App.listDocuments().values():
        filename = dest / (doc.Name + f".recovery-{time.time_ns()}-{uuid.uuid4().hex[:8]}.FCStd")
        doc.saveCopy(str(filename))
        paths.append(str(filename.relative_to(project_root())))
    return {"recovery_files": paths}


def restore():
    manifest = storage() / "session.json"
    if manifest.exists():
        root = project_root()
        saved = json.loads(manifest.read_text())
        active = None
        for name in saved.get("files", []):
            filename = (root / name).resolve()
            if not filename.is_relative_to(root) or filename.suffix.lower() != ".fcstd":
                raise ValueError("Invalid saved project path")
            if filename.is_file():
                doc = App.openDocument(str(filename))
                if name == saved.get("active_file"):
                    active = doc
        if active:
            App.setActiveDocument(active.Name)
    if App.ActiveDocument:
        Gui.activeDocument().activeView().viewAxonometric()
        Gui.activeDocument().activeView().fitAll()


def autosave():
    global _autosave_error
    try:
        save_all()
        _autosave_error = None
    except Exception as exc:
        _autosave_error = str(exc)


def edit(name, expected_revision, code):
    doc = _document(name, expected_revision)
    backup = storage() / ("before-" + uuid.uuid4().hex + ".FCStd")
    doc.saveCopy(str(backup))
    window = Gui.getMainWindow()
    window.setEnabled(False)
    doc.openTransaction("Workspace edit")
    try:
        exec(code, {"App": App, "Gui": Gui, "doc": doc, "__name__": "__main__"})
        doc.recompute()
        invalid = [o.Name for o in doc.Objects if "Invalid" in o.State]
        if invalid:
            raise ValueError("Invalid objects after recompute: " + ", ".join(invalid))
        doc.commitTransaction()
    except BaseException:
        doc.abortTransaction()
        raise
    finally:
        window.setEnabled(True)
    saved = save_all()
    return {"state": state(), "saved": saved, "backup": str(backup.relative_to(project_root()))}


def export_object(name, expected_revision, object_name, format):
    import Part
    import MeshPart
    doc = _document(name, expected_revision)
    obj = doc.getObject(object_name)
    if obj is None or not hasattr(obj, "Shape"):
        raise ValueError("Choose an existing object with geometry")
    _solid(obj)
    if format not in ("step", "stl"):
        raise ValueError("Supported formats: step, stl")
    save_all()
    filename = storage() / (object_name + "-" + uuid.uuid4().hex[:8] + "." + format)
    if format == "step":
        Part.export([obj], str(filename))
    else:
        mesh = MeshPart.meshFromShape(Shape=obj.Shape, LinearDeflection=0.05, AngularDeflection=0.1, Relative=False)
        mesh.write(str(filename))
    return {"file": str(filename.relative_to(project_root())), "unit": "mm", "document_revision": revision(doc)}


def view():
    if not App.ActiveDocument:
        raise ValueError("No active document")
    filename = storage() / ("view-" + uuid.uuid4().hex + ".png")
    Gui.activeDocument().activeView().saveImage(str(filename), 1280, 720, "Current")
    return {"file": str(filename.relative_to(project_root())), "revision": revision(App.ActiveDocument)}
