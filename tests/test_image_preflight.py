import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('preflight', ROOT / 'scripts/image_preflight.py')
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)
SIGNED = b'-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\nSuite: noble\nSHA256:\n abc 3 file\n-----BEGIN PGP SIGNATURE-----\n'


class ImagePreflightTests(unittest.TestCase):
    def curl(self, body, code=0):
        def run(args, **kwargs):
            self.assertNotIn('--head', args)
            self.assertEqual(kwargs['timeout'], 20)
            self.assertIn('--max-time', args)
            self.assertIn('--max-filesize', args)
            Path(args[args.index('--output') + 1]).write_bytes(body)
            return subprocess.CompletedProcess(args, code, b'', b'HTTP 503')
        return run

    def test_get_failure_rejected_even_if_body_looks_valid(self):
        with patch.object(p.subprocess, 'run', self.curl(SIGNED, 22)):
            self.assertFalse(p.check(('snapshot', 'https://example.org'))['ok'])

    def test_html_200_is_not_availability(self):
        with patch.object(p.subprocess, 'run', self.curl(b'<html>proxy error</html>')):
            self.assertFalse(p.check(('snapshot', 'https://example.org'))['ok'])

    def test_signed_metadata_passes(self):
        with patch.object(p.subprocess, 'run', self.curl(SIGNED)):
            self.assertTrue(p.check(('snapshot', 'https://example.org'))['ok'])

    def test_timeout_fails_closed(self):
        with patch.object(p.subprocess, 'run', side_effect=subprocess.TimeoutExpired('probe', 20)):
            self.assertEqual(p.check(('manifest', 'image'))['error'], 'probe timeout')

    def test_invalid_manifest_rejected(self):
        for content in (b'{}', b'[]', b'null', b'<html>error</html>'):
            with self.subTest(content=content), patch.object(p.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, content, b'')):
                self.assertFalse(p.check(('manifest', 'image'))['ok'])

    def test_unpinned_and_unrecognized_sources_rejected(self):
        dockerfile = (ROOT / 'Dockerfile').read_text()
        for content in [dockerfile.replace('@sha256:', '@sha512:', 1),
                        dockerfile.replace('https://snapshot.ubuntu.com', 'https://private.example'),
                        dockerfile.replace('check-valid-until=no', 'trusted=yes', 1)]:
            with self.subTest(content=content[:50]), self.assertRaises(ValueError):
                p.inputs(content)

    def test_current_inputs_include_each_snapshot_and_base(self):
        targets = p.inputs((ROOT / 'Dockerfile').read_text())
        self.assertEqual(sum(kind == 'manifest' for kind, _ in targets), 4)
        self.assertEqual(sum(kind == 'snapshot' for kind, _ in targets), 5)
        self.assertTrue(any('/dists/noble-security/InRelease' in url for _, url in targets))


if __name__ == '__main__':
    unittest.main()
