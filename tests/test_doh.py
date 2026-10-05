import importlib.util
import io
import json
import os
import pathlib
import socket
import unittest
import urllib.request
from unittest.mock import patch, Mock

spec = importlib.util.spec_from_file_location('egress', pathlib.Path(__file__).resolve().parents[1] / 'egress.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class DoH(unittest.TestCase):
    def test_https_dns_opener_rejects_redirects(self):
        with patch.dict(os.environ, {'PUBLIC_EGRESS_DOH_URL': 'https://dns.alidns.com/resolve'}), patch.object(urllib.request.OpenerDirector, 'open') as open_request:
            open_request.side_effect = OSError('network deliberately disabled')
            captured = []
            original = urllib.request.build_opener
            def build(*handlers):
                opener = original(*handlers)
                captured.append(opener)
                return opener
            with patch.object(urllib.request, 'build_opener', side_effect=build):
                with self.assertRaises(OSError):
                    m.connect('github.com', 443)
            handler = next(h for h in captured[0].handlers if isinstance(h, urllib.request.HTTPRedirectHandler))
            request = urllib.request.Request('https://dns.alidns.com/resolve?name=github.com')
            for target in ['http://dns.alidns.com/resolve', 'https://other.example/resolve']:
                with self.subTest(target=target):
                    try:
                        redirected = handler.redirect_request(request, None, 302, 'Found', {}, target)
                    except urllib.error.HTTPError:
                        redirected = None
                    self.assertIsNone(redirected, 'DNS resolver followed a redirect')
    def test_explicit_https_resolver_bypasses_synthetic_system_answers(self):
        fake = [(socket.AF_INET, socket.SOCK_STREAM, 6, '', ('198.18.0.11', 443))]
        opener = Mock()
        opener.open.side_effect = [io.BytesIO(json.dumps({'Status': 0, 'Answer': [{'type': 1, 'data': '20.85.130.105'}]}).encode()), io.BytesIO(b'{"Status":0,"Answer":[]}')]
        with patch.dict(os.environ, {'PUBLIC_EGRESS_DOH_URL': 'https://dns.alidns.com/resolve'}), patch.object(m.socket, 'getaddrinfo', return_value=fake), patch.object(urllib.request, 'build_opener', return_value=opener), patch.object(m.socket, 'socket') as dial:
            m.connect('broker.actions.githubusercontent.com', 443)
            dial.return_value.connect.assert_called_once_with(('20.85.130.105', 443))
            self.assertEqual(opener.open.call_count, 2)

    def test_mixed_public_private_https_answers_fail_closed(self):
        opener = Mock()
        opener.open.side_effect = [io.BytesIO(b'{"Status":0,"Answer":[{"type":1,"data":"1.1.1.1"},{"type":1,"data":"127.0.0.1"}]}'), io.BytesIO(b'{"Status":0}')]
        with patch.dict(os.environ, {'PUBLIC_EGRESS_DOH_URL': 'https://dns.alidns.com/resolve'}), patch.object(urllib.request, 'build_opener', return_value=opener), patch.object(m.socket, 'socket') as dial:
            with self.assertRaises(ValueError):
                m.connect('mixed.example', 443)
            dial.assert_not_called()

    def test_https_dns_failure_does_not_fall_back_to_system_answers(self):
        opener = Mock()
        opener.open.side_effect = OSError('resolver unavailable')
        with patch.dict(os.environ, {'PUBLIC_EGRESS_DOH_URL': 'https://dns.alidns.com/resolve'}), patch.object(urllib.request, 'build_opener', return_value=opener), patch.object(m.socket, 'getaddrinfo') as system_dns, patch.object(m.socket, 'socket') as dial:
            with self.assertRaises(OSError):
                m.connect('broker.actions.githubusercontent.com', 443)
            system_dns.assert_not_called()
            dial.assert_not_called()

    def test_numeric_private_target_does_not_query_https_resolver(self):
        with patch.dict(os.environ, {'PUBLIC_EGRESS_DOH_URL': 'https://dns.alidns.com/resolve'}), patch.object(urllib.request, 'build_opener') as resolver:
            with self.assertRaises(ValueError):
                m.connect('192.168.110.67', 443)
            resolver.assert_not_called()

    def test_unsafe_resolver_url_fails_before_network_access(self):
        for endpoint in ['http://dns.alidns.com/resolve', 'https://u:p@dns.alidns.com/resolve', 'https://dns.alidns.com/resolve?name=x', 'https://dns.alidns.com:8443/resolve']:
            with self.subTest(endpoint=endpoint), patch.dict(os.environ, {'PUBLIC_EGRESS_DOH_URL': endpoint}), patch.object(urllib.request, 'build_opener') as resolver:
                with self.assertRaises(ValueError):
                    m.connect('github.com', 443)
                resolver.assert_not_called()
