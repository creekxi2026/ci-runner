"""Legacy protected pools must fail before touching the runner workspace."""
import os
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]
RETIRED = ('CI_JOB_INPUTS', 'CI_JOB_INTAKE', 'CI_INTAKE_BOOTSTRAP_FD',
           'CI_INTAKE_DIAGNOSTICS')


class RetiredInputTests(unittest.TestCase):
    def test_legacy_settings_reject_before_network_or_workspace_setup(self):
        for name in RETIRED:
            with self.subTest(name=name):
                env = {k: v for k, v in os.environ.items() if k not in RETIRED}
                env[name] = '1'
                result = subprocess.run(['sh', str(ROOT / 'runner.sh')],
                                        env=env, capture_output=True, timeout=3)
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stdout, b'')
                self.assertIn(b'Retired job-input configuration', result.stderr)
