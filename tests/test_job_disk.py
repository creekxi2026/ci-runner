import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location('job_disk', Path(__file__).resolve().parents[1] / 'job-disk-init.py')
disk = importlib.util.module_from_spec(spec)
spec.loader.exec_module(disk)


class JobDiskTests(unittest.TestCase):
    def test_rejects_nonempty_and_symlink_without_touching_contents(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / 'disk'
            root.mkdir()
            payload = root / 'keep'
            payload.write_text('existing')
            link = Path(tmp) / 'link'
            link.symlink_to(root, target_is_directory=True)
            with patch.object(disk.os, 'chown') as chown:
                for path in (root, link):
                    with self.assertRaises(ValueError):
                        disk.initialize(path)
                chown.assert_not_called()
            self.assertEqual(payload.read_text(), 'existing')
            self.assertEqual(list(root.iterdir()), [payload])

    def test_low_disk_fails_before_creating_directories(self):
        with tempfile.TemporaryDirectory() as tmp:
            with patch.object(disk.os, 'statvfs', return_value=SimpleNamespace(f_bavail=0, f_frsize=4096)):
                with self.assertRaisesRegex(ValueError, '5 GiB'):
                    disk.initialize(tmp)
            self.assertEqual(list(Path(tmp).iterdir()), [])
