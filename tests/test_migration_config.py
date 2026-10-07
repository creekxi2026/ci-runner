import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]

class MigrationConfiguration(unittest.TestCase):
    def test_removed_business_api_is_not_distributed_or_used_by_core(self):
        # Known migration regressions only; semantic neutrality needs review.
        files = [p for p in ROOT.glob('*.go') if not p.name.endswith('_test.go')]
        files += list(ROOT.glob('*.py')) + list(ROOT.glob('*.sh'))
        files += [ROOT / 'Dockerfile', ROOT / 'compose.yaml', ROOT / '.env.example']
        files += list((ROOT / 'scripts').glob('*.py'))
        files += list((ROOT / '.github/workflows').glob('*.yml'))
        forbidden = re.compile(
            r'ci-postgres|CI_POSTGRES_IMAGE|POSTGRES_IMAGE|'
            r'CLOSET_PG_[A-Z_]+|CI_DATABASE_[A-Z_]+|closet_ai_test_')
        for path in files:
            with self.subTest(path=str(path.relative_to(ROOT))):
                self.assertIsNone(forbidden.search(path.name))
                self.assertIsNone(forbidden.search(path.read_text()))

    def test_runner_image_contains_generic_rsync_dependency(self):
        runner = (ROOT / 'Dockerfile').read_text().split(' AS runner', 1)[1]
        packages = re.search(r'apt-get install -y --no-install-recommends ([^&]+)', runner).group(1).split()
        self.assertIn('rsync', packages)

    def test_operator_configs_preserve_manual_and_generic_defaults(self):
        compose = (ROOT / 'compose.yaml').read_text()
        example = (ROOT / '.env.example').read_text()
        self.assertIn('RUNNER_ALLOWED_EVENTS: ${RUNNER_ALLOWED_EVENTS:-workflow_dispatch}', compose)
        self.assertIn('SERVICES_CATALOG_FILE: /etc/ci-services/catalog.json', compose)
        self.assertIn('SERVICES_POLICY_FILE: /etc/ci-services/policy.json', compose)
        self.assertNotIn('POSTGRES_', compose)
        self.assertIn('RUNNER_ALLOWED_EVENTS=workflow_dispatch\n', example)
        self.assertIn('SERVICES_CONFIG_DIR=./services\n', example)
