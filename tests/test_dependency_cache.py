import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]

class DependencyCacheTests(unittest.TestCase):
    def load(self):
        path = ROOT / 'cache-init.py'
        self.assertTrue(path.exists(), 'cache initializer missing')
        spec = importlib.util.spec_from_file_location('cache_init', path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_initializes_only_fixed_directory_metadata(self):
        module = self.load()
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            module.initialize(str(root), os.getuid(), os.getgid())
            for name in ('npm', 'pip', 'gomod', 'go-build'):
                self.assertEqual((root / name).stat().st_mode & 0o777, 0o700)
            payload = root / 'npm' / 'payload'
            payload.write_text('cached')
            payload.chmod(0o400)
            module.initialize(str(root), os.getuid(), os.getgid())
            self.assertEqual(payload.stat().st_mode & 0o777, 0o400)
            self.assertEqual(payload.read_text(), 'cached')

    def test_full_cache_refuses_new_jobs_without_deleting_entries(self):
        module = self.load()
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            (root / 'npm').mkdir()
            payload = root / 'npm' / 'payload'
            payload.write_bytes(b'12345')
            with self.assertRaises(OSError):
                module.initialize(str(root), os.getuid(), os.getgid(), max_bytes=5)
            self.assertEqual(payload.read_bytes(), b'12345')
            self.assertFalse((root / 'pip').exists())

    def test_image_and_controller_opt_in_wiring(self):
        self.assertTrue(any(line.startswith('COPY ') and 'cache-init.py' in line.split()[1:-1] and line.split()[-1] == '/opt/ci/' for line in (ROOT / 'Dockerfile').read_text().splitlines()))
        controller = (ROOT / 'controller.go').read_text()
        self.assertIn('f.prepareDependencyCache(ctx, name, h, env)', controller)
        self.assertIn('os.Getenv("DEPENDENCY_CACHE_MODE")', controller)
        compose = (ROOT / 'compose.yaml').read_text()
        self.assertIn('DEPENDENCY_CACHE_MODE: ${DEPENDENCY_CACHE_MODE:-off}', compose)
        self.assertIn('npm_config_cache=/home/runner/.npm', (ROOT / 'cache.go').read_text())
        self.assertIn('. /opt/ci/cache-env.sh', (ROOT / 'runner.sh').read_text())
        self.assertIn('DEPENDENCY_CACHE_VOLUME:', compose)
        self.assertIn('DEPENDENCY_CACHE_TRUST_LANE:', compose)

    def test_symlink_preflight_has_no_partial_ownership_changes(self):
        module = self.load()
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as outside:
            root = Path(root)
            (root / 'npm').mkdir(mode=0o755)
            (root / 'go-build').symlink_to(outside, target_is_directory=True)
            with self.assertRaises(OSError):
                module.initialize(str(root), os.getuid(), os.getgid())
            self.assertEqual((root / 'npm').stat().st_mode & 0o777, 0o755)
            self.assertFalse((root / 'pip').exists())
            with self.assertRaises(OSError):
                module.initialize(str(root / 'go-build'), os.getuid(), os.getgid())

    def test_controller_volume_layout_preserves_cache_and_tool_permissions(self):
        module = self.load()
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            module.initialize_volume(root, os.getuid(), os.getgid())
            payload = root/'data'/'npm'/'_cacache'/'fixture'
            payload.write_text('retained')
            payload.chmod(0o400)
            module.initialize_volume(root, os.getuid(), os.getgid())
            self.assertEqual(payload.read_text(), 'retained')
            self.assertEqual(payload.stat().st_mode & 0o777, 0o400)
            self.assertEqual((root/'data'/'tools').stat().st_mode & 0o777, 0o755)
            self.assertTrue((root/'data'/'jest').is_dir())

    def test_controller_rejects_tools_symlink_and_unknown_layout(self):
        module = self.load()
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as outside:
            root = Path(root)
            (root/'data').mkdir()
            (root/'data'/'tools').symlink_to(outside)
            with self.assertRaises(OSError): module.initialize_volume(root, os.getuid(), os.getgid())
            self.assertFalse((root/'data'/'npm').exists())
            (root/'data'/'tools').unlink()
            (root/'data'/'ready.json').write_text('{"schema":"unknown"}')
            with self.assertRaises(ValueError): module.initialize_volume(root, os.getuid(), os.getgid())
            self.assertFalse((root/'data'/'npm').exists())

if __name__ == '__main__':
    unittest.main()
