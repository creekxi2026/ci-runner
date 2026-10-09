import importlib.util
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('publish_seed', Path(__file__).resolve().parents[1] / 'profiles/cache/publish-seed.py')
publisher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publisher)


class PublisherCleanup(unittest.TestCase):
    def exercise(self, fail=False, tools=True):
        args = ['publish-seed', '--source-volume', 'candidate-cache', '--work-volume', 'candidate-work',
                '--image', 'verified-image', '--provenance', 'reviewed']
        args += ['--tools', '--tool', 'python', '--tool', 'wheels'] if tools else ['--go-namespace', 'go-test']
        def output(*command):
            if command[:3] == ('docker', 'volume', 'inspect'):
                return json.dumps([{'Driver': 'local', 'Options': {}, 'Labels': {
                    'ci-runner.cache-schema': 'v2-linux-arm64-trusted-manual'}}])
            return ''
        def execute(command, **kwargs):
            if fail and command[:2] == ['docker', 'run']:
                raise subprocess.CalledProcessError(1, command)
        with patch('sys.argv', args), patch.object(publisher, 'output', side_effect=output), \
                patch.object(publisher.subprocess, 'run', side_effect=execute) as run:
            if fail:
                with self.assertRaises(subprocess.CalledProcessError):
                    publisher.main()
            else:
                publisher.main()
        calls = [call.args[0] for call in run.call_args_list]
        launched = calls[0][calls[0].index('--name') + 1]
        self.assertTrue(launched.startswith('ci-seed-publish-'))
        self.assertEqual(calls[-1], ['docker', 'rm', '-f', launched])

    def test_tool_success_cleans_exact_publisher(self):
        self.exercise()

    def test_tool_failure_cleans_exact_publisher(self):
        self.exercise(fail=True)

    def test_dependency_success_cleans_exact_publisher(self):
        self.exercise(tools=False)


if __name__ == '__main__':
    unittest.main()
