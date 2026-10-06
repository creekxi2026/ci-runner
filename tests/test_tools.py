import importlib.util
import os
import subprocess
import sys
from unittest.mock import patch
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
        for name in ('python', 'bin', 'custom-sdk'):
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
            one = tools.publish_tools(source, templates, 'reviewed fixture', ['python', 'bin'])
            two = tools.publish_tools(source, templates, 'reviewed fixture', ['python', 'bin'])
            self.assertEqual(one['tools_seed'], two['tools_seed'])
            files = templates / 'tools' / one['tools_seed'] / 'files'
            self.assertEqual((files/'python/python').read_text(), 'python')
            self.assertFalse((files/'custom-sdk').exists())
            self.assertEqual((files/'bin/content').stat().st_mode & 0o222, 0)
            self.assertNotEqual((source/'bin/content').stat().st_ino, (files/'bin/content').stat().st_ino)
            (source/'bin/content').write_text('changed source')
            self.assertEqual((files/'bin/content').read_text(), 'bin')

    def test_writable_source_and_escaping_links_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d); source = self.fixture(root)
            path = source / 'bin/content'
            path.chmod(0o666)
            with self.assertRaises(ValueError): tools.validate_source(source, ['python', 'bin'])
            path.chmod(0o644)
            (source/'bin/escape').symlink_to('../python/content')
            with self.assertRaises(ValueError): tools.validate_source(source, ['python', 'bin'])

    def test_incomplete_existing_publication_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d); source=self.fixture(root)
            templates=root/'templates'; templates.mkdir(mode=0o700)
            result=tools.publish_tools(source, templates, 'reviewed', ['python', 'bin'])
            (templates/'tools'/result['tools_seed']/'files/bin/content').unlink()
            with self.assertRaises(ValueError): tools.publish_tools(source, templates, 'reviewed', ['python', 'bin'])

    def test_arbitrary_selection_and_changed_selection_have_distinct_identity(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d); source = self.fixture(root)
            templates = root / 'templates'; templates.mkdir(mode=0o700)
            first = tools.publish_tools(source, templates, 'reviewed', ['custom-sdk'])
            second = tools.publish_tools(source, templates, 'reviewed', ['bin', 'custom-sdk'])
            self.assertNotEqual(first['tools_seed'], second['tools_seed'])
            self.assertEqual(second['tools_seed'], tools.publish_tools(source, templates, 'reviewed', ['custom-sdk', 'bin'])['tools_seed'])
            self.assertEqual((templates/'tools'/first['tools_seed']/'files/custom-sdk/content').read_text(), 'custom-sdk')

    def test_invalid_selection_cannot_publish(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d); source = self.fixture(root)
            templates = root/'templates'; templates.mkdir(mode=0o700)
            for selected in [[], ['bin','bin'], ['../bin'], ['/bin'], ['.'], ['nested/bin'], ['missing']]:
                with self.subTest(selected=selected), self.assertRaises(ValueError):
                    tools.publish_tools(source, templates, 'reviewed', selected)
            self.assertEqual(list(templates.iterdir()), [])

    def test_wrapper_forwards_selection_and_rejects_invalid_options_before_docker(self):
        path = Path(__file__).resolve().parents[1]/'profiles/cache/publish-seed.py'
        spec = importlib.util.spec_from_file_location('publish_seed', path)
        wrapper = importlib.util.module_from_spec(spec); spec.loader.exec_module(wrapper)
        base = ['publish-seed.py','--source-volume','source-cache','--image','verified-image','--provenance','reviewed']
        def output(*cmd):
            if cmd[1] == 'volume':
                return '[{"Driver":"local","Options":{},"Labels":{"ci-runner.cache-schema":"v2-linux-arm64-trusted-manual"}}]'
            return ''
        with patch.object(sys, 'argv', base+['--tools','--tool','custom-sdk']), patch.object(wrapper, 'output', side_effect=output), patch.object(wrapper.subprocess,'run') as run:
            wrapper.main()
            self.assertEqual(run.call_args_list[0].args[0][-2:], ['--tool','custom-sdk'])
        for options in [['--tools'], ['--tools','--tool','../bin'], ['--tool','bin']]:
            result = subprocess.run([sys.executable,str(path),*base[1:],*options],capture_output=True)
            self.assertNotEqual(result.returncode,0)
            self.assertIn(b'--tool',result.stderr)
