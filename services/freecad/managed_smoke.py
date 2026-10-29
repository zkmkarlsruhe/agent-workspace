# SPDX-License-Identifier: MIT

"""Run inside a configured managed container; creates one acceptance document."""
import json
import os
from pathlib import Path
import sys
from control import call as local_call
import uuid

SESSION = os.environ['WORKSPACE_FREECAD_SESSION']

def call(tool, **args):
    # The HTTP control listener is manager-only; this local probe uses the same facade.
    return local_call(tool, dict(session_id=SESSION, **args))

def value(tool, **args):
    result=call(tool,**args)
    assert not result.get('isError'),result
    return result['structuredContent']

def current(name):
    return next(d for d in value('freecad_state')['documents'] if d['name']==name)

root=Path(os.environ['WORKSPACE_FREECAD_PROJECT'])
marker=root/'managed-smoke.json'
if '--restore' in sys.argv:
    saved=json.loads(marker.read_text())
    doc=next(d for d in value('freecad_state')['documents'] if Path(d['file']).name.startswith(saved['name']+'-'))
    box=next(o for o in doc['objects'] if o['name']=='Base')
    assert box['Height']['value']==6,box
    assert doc['revision']!=saved['revision']
    state=value('freecad_state')
    assert state['autosave_error'] is None
    assert state['active_document']==doc['name'],state['active_document']
    print('PASS: native document, dimensions and new revision restored after container recreation')
    sys.exit(0)

name='Acceptance_'+uuid.uuid4().hex[:8]
value('freecad_document',action='new',name=name)
doc=current(name)
args=dict(document=name,revision=doc['revision'],request_id='create_'+uuid.uuid4().hex,
          code='''base=doc.addObject("Part::Box", "Base")
base.Length=60
base.Width=40
base.Height=4
hole=doc.addObject("Part::Cylinder", "Hole")
hole.Radius=3
hole.Height=6
hole.Placement.Base=App.Vector(15,20,0)
cut=doc.addObject("Part::Cut", "Result")
cut.Base=base
cut.Tool=hole
base.ViewObject.Visibility=False
hole.ViewObject.Visibility=False
Gui.activeDocument().activeView().viewAxonometric()
Gui.activeDocument().activeView().fitAll()''')
result=call('freecad_execute',**args)
assert not result.get('isError'),result
assert call('freecad_execute',**args)==result
first=current(name)
assert len(first['objects'])==3
conflict=call('freecad_execute',**(args|{'request_id':'stale_'+uuid.uuid4().hex,'code':'doc.Base.Height=99'}))
assert conflict.get('isError'),conflict
assert current(name)['revision']==first['revision']
edit=call('freecad_execute',document=name,revision=first['revision'],request_id='edit_'+uuid.uuid4().hex,code='doc.Base.Height=6')
assert not edit.get('isError'),edit
updated=current(name)
cut=next(o for o in updated['objects'] if o['name']=='Result')
assert cut['geometry']['valid'] and cut['geometry']['solids']==1
assert cut['geometry']['size_mm']==[60,40,6],cut
for fmt in ('step','stl'):
    out=value('freecad_export',document=name,revision=updated['revision'],object='Result',format=fmt)
    assert (root/out['file']).stat().st_size>100
view=value('freecad_view')
assert (root/view['file']).read_bytes().startswith(b'\x89PNG')
assert value('freecad_save')['saved']
marker.write_text(json.dumps({'name':name,'revision':updated['revision']}))
print('PASS: new document, parameter edit, duplicate suppression, revision conflict, solid geometry, native save, STEP/STL, PNG')
