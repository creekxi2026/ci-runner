import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location('ci_service', Path(__file__).parents[1] / 'ci-service.py')
cli = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cli)


class ServiceCLI(unittest.TestCase):
    def test_invalid_request_never_opens_metadata(self):
        for args, status in [(['acquire', '../escape'], 65),
                             (['acquire', 'service', '--timeout', '0'], 64),
                             (['exec', 'service'], 64),
                             (['ready', 'service', '--', 'sh'], 64),
                             (['acquire', 'service', '--image', 'untrusted'], 64)]:
            with self.subTest(args=args), patch.object(cli, 'metadata') as read:
                self.assertEqual(cli.run(args), status)
                read.assert_not_called()

    def test_untrusted_mount_fails_before_request(self):
        with patch.dict(os.environ, {'CI_SERVICES_FILE': '/tmp/forged'}), patch.object(cli, 'metadata') as read:
            self.assertEqual(cli.run(['acquire', 'service']), 70)
            read.assert_not_called()

    def test_symlink_metadata_is_rejected(self):
        with tempfile.TemporaryDirectory() as folder:
            real = Path(folder) / 'real'
            real.write_text('{}')
            link = Path(folder) / 'link'
            link.symlink_to(real)
            with self.assertRaises(OSError):
                cli.metadata(link)


if __name__ == '__main__':
    unittest.main()
