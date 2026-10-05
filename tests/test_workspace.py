import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("workspace", Path(__file__).resolve().parents[1] / "workspace-init.py")
workspace = importlib.util.module_from_spec(spec)
spec.loader.exec_module(workspace)


class WorkspaceTest(unittest.TestCase):
    def test_private_copy_changes_do_not_change_source_or_sibling(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            source = root / "source"
            source.mkdir()
            (source / "bytes").write_bytes(b"a" * 100000)
            counts = {"cloned_bytes": 0, "copied_bytes": 0}
            for name in ["one", "two"]:
                workspace.clone_tree(source, root / name, os.getuid(), counts)
            (root / "one/bytes").write_bytes(b"changed")
            self.assertEqual((source / "bytes").read_bytes(), b"a" * 100000)
            self.assertEqual((root / "two/bytes").read_bytes(), b"a" * 100000)
            self.assertEqual(sum(counts.values()), 200000)
            self.assertNotEqual((source / "bytes").stat().st_ino, (root / "two/bytes").stat().st_ino)

    def test_dependency_links_rejected_runner_internal_links_allowed(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            source = root / "source"
            source.mkdir()
            (source / "bytes").write_text("value")
            (source / "link").symlink_to("bytes")
            counts = {"cloned_bytes": 0, "copied_bytes": 0}
            with self.assertRaises(ValueError):
                workspace.clone_tree(source, root / "bad", os.getuid(), counts)
            workspace.clone_tree(source, root / "ok", os.getuid(), counts, link_root=source)
            self.assertTrue((root / "ok/link").is_symlink())
            (source / "escape").symlink_to("../../outside")
            with self.assertRaises(ValueError):
                workspace.clone_tree(source, root / "escape", os.getuid(), counts, link_root=source)

    def test_interrupted_publication_is_not_reused(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            identity = "a" * 64
            stage = root / "runner" / (identity + ".staging")
            stage.mkdir(parents=True)
            (stage / "partial").write_text("incomplete")
            def build(dest):
                self.assertFalse((dest / "partial").exists())
                (dest / "complete").write_text("complete")
                return {"tested": True}
            path = workspace.publish(root, "runner", identity, build)
            self.assertTrue((path / "complete").exists())
            self.assertFalse(stage.exists())
            def never(_):
                raise AssertionError("immutable template unexpectedly rebuilt")
            self.assertEqual(workspace.publish(root, "runner", identity, never), path)

    def test_template_path_escape_rejected(self):
        for value in ["..", "a/../b", "abc", "A" * 64]:
            with self.assertRaises(ValueError):
                workspace.key(value)

    def test_vcs_auth_state_can_be_excluded_from_import(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            source = root / 'source'
            (source / 'cache/vcs').mkdir(parents=True)
            (source / 'cache/vcs/config').write_text('private checkout metadata')
            (source / 'module').write_text('verified package')
            workspace.clone_tree(source, root / 'copy', os.getuid(), {'cloned_bytes':0, 'copied_bytes':0}, exclude={source/'cache/vcs'})
            self.assertFalse((root/'copy/cache/vcs').exists())
            self.assertEqual((root/'copy/module').read_text(), 'verified package')
