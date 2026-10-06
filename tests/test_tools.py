import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('tools', Path(__file__).resolve().parents[1] / 'tools-init.py')
tools = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tools)

class ToolsTest(unittest.TestCase):
    def fixture(self, root):
        source = root / 'source'
        source.mkdir()
        for name in tools.DIRECTORIES:
            (source / name).mkdir(mode=0o755)
            (source / name / 'content').write_text(name)
        (source / 'python/python').symlink_to('content')
        return source

    def test_publish_is_immutable_isolated_and_idempotent(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            source = self.fixture(root)
            templates = root / 'templates'
            templates.mkdir(mode=0o700)
            one = tools.publish_tools(source, templates, 'reviewed fixture')
            two = tools.publish_tools(source, templates, 'reviewed fixture')
            self.assertEqual(one['tools_seed'], two['tools_seed'])
            files = templates / 'tools' / one['tools_seed'] / 'files'
            self.assertEqual((files/'python/python').read_text(), 'python')
            self.assertEqual((files/'bin/content').stat().st_mode & 0o222, 0)
            self.assertNotEqual((source/'bin/content').stat().st_ino, (files/'bin/content').stat().st_ino)
            (source/'bin/content').write_text('changed source')
            self.assertEqual((files/'bin/content').read_text(), 'bin')

    def test_writable_source_and_escaping_links_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d); source = self.fixture(root)
            path = source / 'bin/content'
            path.chmod(0o666)
            with self.assertRaises(ValueError): tools.validate_source(source)
            path.chmod(0o644)
            (source/'bin/escape').symlink_to('../python/content')
            with self.assertRaises(ValueError): tools.validate_source(source)

    def test_incomplete_existing_publication_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d); source=self.fixture(root)
            templates=root/'templates'; templates.mkdir(mode=0o700)
            result=tools.publish_tools(source, templates, 'reviewed')
            (templates/'tools'/result['tools_seed']/'files/bin/content').unlink()
            with self.assertRaises(ValueError): tools.publish_tools(source, templates, 'reviewed')
