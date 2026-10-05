import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]

class MigrationConfiguration(unittest.TestCase):
    def test_runner_image_contains_generic_rsync_dependency(self):
        runner = (ROOT / 'Dockerfile').read_text().split(' AS runner', 1)[1]
        packages = re.search(r'apt-get install -y --no-install-recommends ([^&]+)', runner).group(1).split()
        self.assertIn('rsync', packages)

    def test_operator_configs_preserve_manual_and_generic_defaults(self):
        compose = (ROOT / 'compose.yaml').read_text()
        example = (ROOT / '.env.example').read_text()
        self.assertIn('RUNNER_ALLOWED_EVENTS: ${RUNNER_ALLOWED_EVENTS:-workflow_dispatch}', compose)
        self.assertIn('POSTGRES_DATABASE_PREFIX: ${POSTGRES_DATABASE_PREFIX:-}', compose)
        self.assertIn('RUNNER_ALLOWED_EVENTS=workflow_dispatch\n', example)
        self.assertIn('POSTGRES_DATABASE_PREFIX=\n', example)
