"""Local socket-only tunnel regression tests; never dial external networks."""
import importlib.util
import contextlib
import io
import json
import pathlib
import socket
import threading
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('egress_tunnel', pathlib.Path(__file__).resolve().parents[1] / 'egress.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class Tunnel(unittest.TestCase):
    def setUp(self):
        self.log_stream = io.StringIO()
        capture = contextlib.redirect_stderr(self.log_stream)
        capture.__enter__()
        self.addCleanup(capture.__exit__, None, None, None)

    def test_idle_relay_survives_100_seconds_but_has_finite_budget(self):
        client, proxy = socket.socketpair()
        remote, peer = socket.socketpair()
        clock = [0.0]
        waits = []
        def select_after_idle(readers, writes, errors, timeout):
            waits.append(timeout)
            clock[0] += min(100, timeout)
            if timeout < 100: return [], [], []
            peer.sendall(b'late-tls-record')
            peer.shutdown(socket.SHUT_WR)
            client.shutdown(socket.SHUT_WR)
            # Subsequent readiness uses real sockets, with no wall-clock delay.
            return readers, [], []
        with client, proxy, peer:
            client.sendall(b'CONNECT public.example:443 HTTP/1.1\r\n\r\n')
            original_select = m.select.select
            calls = [0]
            def controlled_select(*args):
                calls[0] += 1
                return select_after_idle(*args) if calls[0] == 1 else original_select(*args)
            with patch.object(m, 'connect', return_value=remote), patch.object(m.time, 'monotonic', side_effect=lambda: clock[0]), patch.object(m.select, 'select', side_effect=controlled_select):
                m.Handler(proxy, ('local', 0), None)
            # Handler propagates the remote FIN on the successful relay path.
            response = b''
            while True:
                data = client.recv(4096)
                if not data: break
                response += data
            self.assertTrue(response.endswith(b'late-tls-record'), response)
            self.assertGreaterEqual(waits[0], 300)
            self.assertLessEqual(waits[0], 3600)

    def test_client_half_close_drains_real_server_tail(self):
        client, proxy = socket.socketpair()
        remote, peer = socket.socketpair()
        errors = []
        def handle():
            try:
                m.Handler(proxy, ('local', 0), None)
            except Exception as error:
                errors.append(error)
            finally:
                proxy.close()
        with client, peer, patch.object(m, 'connect', return_value=remote):
            client.settimeout(2)
            peer.settimeout(2)
            client.sendall(b'CONNECT public.example:443 HTTP/1.1\r\n\r\n')
            thread = threading.Thread(target=handle, daemon=True)
            thread.start()
            try:
                self.assertEqual(client.recv(4096), b'HTTP/1.1 200 Connection Established\r\n\r\n')
                client.sendall(b'request-bytes')
                client.shutdown(socket.SHUT_WR)
                request = b''
                while True:
                    chunk = peer.recv(4096)
                    if not chunk: break
                    request += chunk
                self.assertEqual(request, b'request-bytes')
                # Response starts only after upstream observes propagated FIN.
                try:
                    peer.sendall(b'server-tail' * 1000)
                    peer.shutdown(socket.SHUT_WR)
                except BrokenPipeError:
                    self.fail('proxy closed upstream before server response tail')
                response = b''
                while True:
                    chunk = client.recv(65536)
                    if not chunk: break
                    response += chunk
                self.assertEqual(response, b'server-tail' * 1000)
            finally:
                client.close()
                peer.close()
                thread.join(3)
            self.assertFalse(thread.is_alive())
            self.assertEqual(errors, [])
        record = json.loads(self.log_stream.getvalue())
        self.assertEqual(record['bytes_client_to_server'], 13)
        self.assertEqual(record['bytes_server_to_client'], 11000)
        self.assertEqual(record['eof_directions'], ['client', 'server'])
        self.assertEqual(record['close_reason'], 'eof')
        self.assertNotIn('server-tail', self.log_stream.getvalue())

    def test_server_half_close_still_drains_client_tail(self):
        client, proxy = socket.socketpair()
        remote, peer = socket.socketpair()
        def handle():
            try:
                m.Handler(proxy, ('local', 0), None)
            finally:
                proxy.close()
        with client, peer, patch.object(m, 'connect', return_value=remote):
            client.settimeout(2)
            peer.settimeout(2)
            client.sendall(b'CONNECT public.example:443 HTTP/1.1\r\n\r\n')
            thread = threading.Thread(target=handle, daemon=True)
            thread.start()
            try:
                self.assertEqual(client.recv(4096), b'HTTP/1.1 200 Connection Established\r\n\r\n')
                peer.sendall(b'server-finished')
                peer.shutdown(socket.SHUT_WR)
                response = b''
                while True:
                    data = client.recv(4096)
                    if not data: break
                    response += data
                self.assertEqual(response, b'server-finished')
                client.sendall(b'client-tail')
                client.shutdown(socket.SHUT_WR)
                request = b''
                while True:
                    data = peer.recv(4096)
                    if not data: break
                    request += data
                self.assertEqual(request, b'client-tail')
            finally:
                client.close()
                peer.close()
                thread.join(3)
            self.assertFalse(thread.is_alive())
        record = json.loads(self.log_stream.getvalue())
        self.assertEqual(record['eof_directions'], ['server', 'client'])
        self.assertEqual(record['bytes_client_to_server'], 11)
        self.assertEqual(record['bytes_server_to_client'], 15)

    def test_dns_aaaa_private_or_failed_lookup_remains_fail_closed(self):
        from unittest.mock import Mock
        for aaaa in (io.BytesIO(b'{"Status":0,"Answer":[{"type":28,"data":"fc00::1"}]}'), OSError('AAAA unavailable')):
            with self.subTest(aaaa=type(aaaa).__name__):
                opener = Mock()
                opener.open.side_effect = [io.BytesIO(b'{"Status":0,"Answer":[{"type":1,"data":"1.1.1.1"}]}'), aaaa]
                with patch.dict(m.os.environ, {'PUBLIC_EGRESS_DOH_URL': 'https://resolver.example/resolve'}), patch.object(m.urllib.request, 'build_opener', return_value=opener), patch.object(m.socket, 'socket') as dial:
                    with self.assertRaises((ValueError, OSError)):
                        m.connect('public.example', 443)
                    dial.assert_not_called()

    def test_connect_diagnostics_use_only_validated_pinned_public_ip(self):
        from unittest.mock import Mock
        for address in ('1.1.1.1', '2606:4700:4700::1111', 'fc00::1'):
            with self.subTest(address=address):
                diagnostic = {}
                answer = [(socket.AF_INET6 if ':' in address else socket.AF_INET, socket.SOCK_STREAM, 6, '', (address, 443))]
                with patch.object(m, 'resolve', return_value=answer), patch.dict(m.os.environ, {'PUBLIC_EGRESS_UPSTREAM_PROXY': ''}), patch.object(m.socket, 'socket', return_value=Mock()) as dial:
                    if address == 'fc00::1':
                        with self.assertRaises(ValueError): m.connect('public.example', 443, diagnostic)
                        self.assertNotIn('peer_ip', diagnostic)
                        self.assertEqual(diagnostic['phase'], 'policy')
                        dial.assert_not_called()
                    else:
                        m.connect('public.example', 443, diagnostic)
                        self.assertEqual(diagnostic['peer_ip'], address)
                        self.assertEqual(diagnostic['phase'], 'dial')

    def test_http_url_query_and_header_credentials_never_enter_log(self):
        client, proxy = socket.socketpair()
        remote, peer = socket.socketpair()
        with client, proxy, peer:
            client.sendall(b'GET http://public.example/private-path?query-marker HTTP/1.1\r\nAuthorization: credential-marker\r\n\r\n')
            with patch.object(m, 'connect', return_value=remote), patch.object(m.select, 'select', side_effect=OSError('secret-marker')):
                m.Handler(proxy, ('local', 0), None)
        record = json.loads(self.log_stream.getvalue())
        self.assertEqual(record['hostname'], 'public.example')
        self.assertEqual(record['phase'], 'relay')
        for marker in ('private-path', 'query-marker', 'credential-marker', 'http://', 'secret-marker'):
            self.assertNotIn(marker, self.log_stream.getvalue())

    def test_idle_timeout_is_bounded_and_classified_without_http_after_200(self):
        import contextlib
        import io
        import json
        output = io.StringIO()
        clock = [0.0]
        client, proxy = socket.socketpair()
        remote, peer = socket.socketpair()
        def idle(readers, writes, errors, timeout):
            self.assertEqual(timeout, 300)
            clock[0] += timeout
            return [], [], []
        with client, proxy, peer, contextlib.redirect_stderr(output):
            client.sendall(b'CONNECT public.example:443 HTTP/1.1\r\n\r\n')
            with patch.object(m, 'connect', return_value=remote), patch.object(m.time, 'monotonic', side_effect=lambda: clock[0]), patch.object(m.select, 'select', side_effect=idle):
                m.Handler(proxy, ('local', 0), None)
            proxy.shutdown(socket.SHUT_WR)
            self.assertEqual(client.recv(4096), b'HTTP/1.1 200 Connection Established\r\n\r\n')
            self.assertEqual(client.recv(4096), b'')
        record = json.loads(output.getvalue())
        self.assertEqual(record['close_reason'], 'idle_timeout')
        self.assertEqual(record['error_type'], 'TimeoutError')
        self.assertEqual(record['duration_seconds'], 300)
        self.assertEqual(record['last_io_seconds'], 0)

    def test_dns_invalid_or_empty_lookup_fails_closed_as_502(self):
        for result in (ValueError('invalid DNS secret-marker'), []):
            with self.subTest(result=type(result).__name__):
                client, proxy = socket.socketpair()
                with client, proxy:
                    client.sendall(b'CONNECT public.example:443 HTTP/1.1\r\n\r\n')
                    with patch.object(m, 'resolve', side_effect=result if isinstance(result, Exception) else None, return_value=result), patch.object(m.socket, 'socket') as dial:
                        m.Handler(proxy, ('local', 0), None)
                    proxy.shutdown(socket.SHUT_WR)
                    self.assertIn(b'502 Bad Gateway', client.recv(4096))
                    dial.assert_not_called()

    def test_real_connect_preserves_dial_timeout_for_504(self):
        from unittest.mock import Mock
        answer = [(socket.AF_INET, socket.SOCK_STREAM, 6, '', ('1.1.1.1', 443))]
        dial = Mock()
        dial.connect.side_effect = TimeoutError('secret-marker')
        with patch.object(m, 'resolve', return_value=answer), patch.dict(m.os.environ, {'PUBLIC_EGRESS_UPSTREAM_PROXY': ''}), patch.object(m.socket, 'socket', return_value=dial):
            with self.assertRaises(TimeoutError):
                m.connect('public.example', 443)
        dial.close.assert_called_once()

    def test_diagnostic_hostname_rejects_invalid_or_overlong_labels(self):
        import contextlib
        import io
        import json
        for host in ['public.example..', 'a' * 64 + '.example', '.'.join(['a' * 63] * 4), 'credential%40marker.example']:
            with self.subTest(host=host):
                output = io.StringIO()
                client, proxy = socket.socketpair()
                with client, proxy, contextlib.redirect_stderr(output):
                    client.sendall(('CONNECT ' + host + ':443 HTTP/1.1\r\n\r\n').encode('ascii'))
                    with patch.object(m, 'connect', side_effect=ValueError('denied')):
                        m.Handler(proxy, ('local', 0), None)
                self.assertIsNone(json.loads(output.getvalue())['hostname'])

    def test_safe_default_diagnostics_emit_one_record_without_secrets(self):
        import contextlib
        import io
        import json
        client, proxy = socket.socketpair()
        remote, peer = socket.socketpair()
        output = io.StringIO()
        def dial(host, port, diagnostic=None):
            if diagnostic is not None:
                diagnostic.update(peer_ip='1.1.1.1', phase='dial')
            return remote
        with client, proxy, peer, contextlib.redirect_stderr(output):
            client.sendall(b'CONNECT public.example:443 HTTP/1.1\r\nAuthorization: credential-marker\r\nProxy-Authorization: credential-marker\r\n\r\n')
            with patch.object(m, 'connect', side_effect=dial), patch.object(m.select, 'select', side_effect=ConnectionResetError('https://public.example/path?query-marker credential-marker')):
                m.Handler(proxy, ('local', 0), None)
        lines = output.getvalue().splitlines()
        self.assertEqual(len(lines), 1, output.getvalue())
        record = json.loads(lines[0])
        self.assertEqual(record['event'], 'egress_close')
        self.assertEqual(record['hostname'], 'public.example')
        self.assertEqual(record['peer_ip'], '1.1.1.1')
        self.assertEqual(record['port'], 443)
        self.assertEqual(record['phase'], 'relay')
        self.assertEqual(record['error_type'], 'ConnectionResetError')
        self.assertEqual(record['close_reason'], 'error')
        self.assertEqual(record['close_direction'], 'both')
        self.assertEqual(record['bytes_client_to_server'], 0)
        self.assertEqual(record['bytes_server_to_client'], 0)
        self.assertGreaterEqual(record['duration_seconds'], 0)
        self.assertGreaterEqual(record['last_io_seconds'], 0)
        for marker in ['credential-marker', 'query-marker', '/path', 'https://', 'Authorization']:
            self.assertNotIn(marker, output.getvalue())

    def test_pre_connect_policy_dns_and_transport_statuses(self):
        for error, expected in [(ValueError('denied'), b'403 Forbidden'), (socket.gaierror('dns-secret'), b'502 Bad Gateway'), (ConnectionRefusedError('transport-secret'), b'502 Bad Gateway'), (TimeoutError('timeout-secret'), b'504 Gateway Timeout')]:
            with self.subTest(error=type(error).__name__):
                client, proxy = socket.socketpair()
                with client, proxy:
                    client.sendall(b'CONNECT public.example:443 HTTP/1.1\r\n\r\n')
                    with patch.object(m, 'connect', side_effect=error):
                        m.Handler(proxy, ('local', 0), None)
                    proxy.shutdown(socket.SHUT_WR)
                    self.assertIn(expected, client.recv(4096))

    def test_relay_error_after_connect_never_writes_http(self):
        client, proxy = socket.socketpair()
        remote, peer = socket.socketpair()
        with client, proxy, peer:
            client.sendall(b'CONNECT public.example:443 HTTP/1.1\r\n\r\n')
            with patch.object(m, 'connect', return_value=remote), patch.object(m.select, 'select', side_effect=OSError('secret-marker')):
                m.Handler(proxy, ('local', 0), None)
            proxy.shutdown(socket.SHUT_WR)
            response = b''
            while True:
                chunk = client.recv(4096)
                if not chunk:
                    break
                response += chunk
            self.assertEqual(response, b'HTTP/1.1 200 Connection Established\r\n\r\n')


if __name__ == '__main__':
    unittest.main()
