# SPDX-License-Identifier: MIT

import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from recovery import recover


class RecoveryTests(unittest.TestCase):
    def test_durable_documents_copied_without_following_symlinks(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "part.FCStd").write_bytes(b"document")
            (root / "part.FCStd1").write_bytes(b"previous")
            (root / "link.FCStd").symlink_to(root / "part.FCStd")
            (root / "dir").symlink_to(root, target_is_directory=True)
            copies = recover(root)
            self.assertEqual(len(copies), 2)
            self.assertEqual({(root / c).read_bytes() for c in copies}, {b"document", b"previous"})
            self.assertTrue(all(".recovery-" in c and c.endswith(".FCStd") for c in copies))
            self.assertEqual(len(recover(root)), 2)

    def test_failed_copy_does_not_publish_recovery(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "part.FCStd").write_bytes(b"original")
            with patch("recovery.shutil.copyfileobj", side_effect=OSError("disk full")), self.assertRaises(OSError):
                recover(root)
            self.assertEqual(list(root.iterdir()), [root / "part.FCStd"])
            self.assertEqual((root / "part.FCStd").read_bytes(), b"original")

    def test_live_recovery_during_pending_edit(self):
        with tempfile.TemporaryDirectory() as directory:
            doc = SimpleNamespace(Name="Part", FileName="", HasPendingTransaction=True)
            doc.saveCopy = lambda name: Path(name).write_bytes(b"unsaved geometry")
            app = SimpleNamespace(addDocumentObserver=lambda _: None, listDocuments=lambda: {"Part": doc})
            gui = SimpleNamespace(getDocument=lambda _: SimpleNamespace(getInEdit=lambda: True))
            with patch.dict(sys.modules, FreeCAD=app, FreeCADGui=gui), \
                 patch.dict(os.environ, WORKSPACE_FREECAD_PROJECT=directory):
                spec = importlib.util.spec_from_file_location("recovery_workspace", Path(__file__).with_name("workspace_cad.py"))
                module = importlib.util.module_from_spec(spec)
                spec.loader.exec_module(module)
                result = module.recover_all()
            self.assertEqual(len(result["recovery_files"]), 1)
            self.assertEqual(Path(directory, result["recovery_files"][0]).read_bytes(), b"unsaved geometry")
            self.assertTrue(doc.HasPendingTransaction)

    def test_desktop_uses_exec_delivered_token(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            token = "a" * 64
            (root / "freecad-token").write_text(token)
            script = Path(__file__).with_name("start-desktop").read_text().replace("/run/freecad-token", str(root / "freecad-token"))
            (root / "start").write_text(script)
            selkies = root / "selkies"
            selkies.write_text('#!/bin/sh\nprintf "%s\\n" "$SELKIES_BASIC_AUTH_USER" "$SELKIES_BASIC_AUTH_PASSWORD" "$@"\n')
            selkies.chmod(0o700)
            env = os.environ | {"PATH": directory + ":" + os.environ["PATH"], "WORKSPACE_FREECAD_SESSION": "session"}
            output = subprocess.check_output(["sh", str(root / "start"), "--port", "3000"], env=env, text=True)
            self.assertEqual(output.splitlines(), ["workspace", token, "session", "--enable-basic-auth", "true", "--port", "3000"])


if __name__ == "__main__":
    unittest.main()
