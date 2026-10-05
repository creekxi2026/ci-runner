import contextlib
import io
import os
import pathlib
import runpy
import unittest
import urllib.error
import urllib.parse
from unittest.mock import patch

PROBE = pathlib.Path(__file__).resolve().parents[1] / 'scripts/network_probe.py'
ALIASES = {'host.docker.internal', 'host.orb.internal'}


class NetworkProbe(unittest.TestCase):
    def run_probe(self, override=None):
        seen = []
        class Opener:
            def open(self, url, timeout):
                host = urllib.parse.urlsplit(url).hostname
                seen.append((host, os.environ.get('no_proxy'), os.environ.get('NO_PROXY')))
                code = override.get(host, 403) if override else (502 if host in ALIASES else 403)
                if isinstance(code, Exception):
                    raise code
                if code == 200:
                    return contextlib.nullcontext()
                raise urllib.error.HTTPError(url, code, 'fixture', {}, None)
        output = io.StringIO()
        with patch.dict(os.environ, {'http_proxy': 'http://proxy.example:3128', 'https_proxy': 'http://proxy.example:3128', 'no_proxy': '127.0.0.1', 'NO_PROXY': '127.0.0.1'}), patch('socket.create_connection', side_effect=OSError('blocked')), patch('urllib.request.build_opener', return_value=Opener()), contextlib.redirect_stdout(output):
            runpy.run_path(str(PROBE), run_name='__main__')
        return output.getvalue(), seen

    def test_unresolvable_host_alias_is_not_counted_as_policy_denial(self):
        output, seen = self.run_probe()
        self.assertIn('PROXY HOSTNAME UNREACHABLE host.docker.internal', output)
        self.assertNotIn('PROXY DENIED host.docker.internal', output)
        self.assertIn('PROXY DENIED 192.168.1.1', output)
        self.assertEqual(len(seen), 6)

    def test_numeric_private_destination_still_requires_403(self):
        for code in (502, 504):
            with self.subTest(code=code), self.assertRaises(AssertionError):
                self.run_probe({'192.168.1.1': code})

    def test_any_successful_forbidden_proxy_connection_fails(self):
        for host in ('192.168.1.1', 'host.docker.internal'):
            with self.subTest(host=host), self.assertRaisesRegex(SystemExit, 'PROXY BYPASS'):
                self.run_probe({host: 200})

    def test_transport_error_is_not_numeric_policy_proof(self):
        with self.assertRaises(AssertionError):
            self.run_probe({'127.0.0.1': urllib.error.URLError('refused')})

    def test_explicit_proxy_probe_cannot_bypass_via_no_proxy(self):
        _, seen = self.run_probe({host: 403 for host in ALIASES})
        self.assertTrue(all(lower == '' and upper == '' for _, lower, upper in seen))
