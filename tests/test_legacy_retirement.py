import importlib.util
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]


class LegacyRetirement(unittest.TestCase):
    def test_retirement_requires_exact_owner_and_source_repository(self):
        path = ROOT / 'scripts/retire_legacy_package.py'
        self.assertTrue(path.is_file(), 'bounded legacy retirement script is missing')
        spec = importlib.util.spec_from_file_location('retire_legacy', path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        metadata = {'name': 'ci-runner', 'package_type': 'container',
                    'owner': {'login': 'creekxi2026'},
                    'repository': {'full_name': 'creekxi2026/ci-runner'}}
        self.assertEqual(module.retirement_path('creekxi2026/ci-runner', metadata),
                         'users/creekxi2026/packages/container/ci-runner')
        for key, value in [('name', 'other'), ('package_type', 'npm'),
                           ('owner', {'login': 'other'}),
                           ('repository', None),
                           ('repository', {'full_name': 'creekxi2026/other'})]:
            with self.subTest(key=key, value=value):
                with self.assertRaises(ValueError):
                    module.retirement_path('creekxi2026/ci-runner', {**metadata, key: value})
        with self.assertRaises(ValueError):
            module.retirement_path('creekxi2026/other', metadata)


if __name__ == '__main__':
    unittest.main()
