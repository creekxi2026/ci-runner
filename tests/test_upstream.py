import importlib.util,pathlib,socket,unittest,os
from unittest.mock import patch,MagicMock
spec=importlib.util.spec_from_file_location('egress_upstream',pathlib.Path(__file__).resolve().parents[1]/'egress.py')
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
class Upstream(unittest.TestCase):
 def test_slow_response_cannot_extend_total_handshake_deadline(self):
  conn=MagicMock();clock=[0.0];header=iter(b'HTTP/1.1 200 OK\r\n\r\n')
  def recv(n):
   clock[0]+=4.0
   return bytes([next(header)])
  conn.recv.side_effect=recv
  with patch('time.monotonic',side_effect=lambda:clock[0]),patch.object(m.socket,'create_connection',return_value=conn):
   with self.assertRaises(TimeoutError):m.upstream_tunnel(('1.1.1.1',443),'http://trusted-gateway:7897')
  conn.close.assert_called_once()
  self.assertLessEqual(clock[0],16.0)

 def test_ipv6_tunnel_preserves_data_after_header(self):
  client,server=socket.socketpair()
  with client,server:
   server.sendall(b'HTTP/1.1 200 OK\r\n\r\nNEXT')
   with patch.object(m.socket,'create_connection',return_value=client):
    self.assertIs(m.upstream_tunnel(('2606:4700:4700::1111',443),'http://trusted-gateway:7897'),client)
   self.assertEqual(client.recv(4),b'NEXT')
   self.assertEqual(client.gettimeout(),15)
   self.assertIn(b'CONNECT [2606:4700:4700::1111]:443 HTTP/1.1',server.recv(4096))
 def test_non_http_success_status_is_rejected_and_closed(self):
  for line in (b'NOTHTTP 200\r\n\r\n',b'HTTP/2.0 200 OK\r\n\r\n',b'HTTP/1.2 200 OK\r\n\r\n'):
   with self.subTest(line=line):
    conn=MagicMock();conn.recv.side_effect=[bytes([x]) for x in line]
    with patch.object(m.socket,'create_connection',return_value=conn):
     with self.assertRaises(OSError):m.upstream_tunnel(('1.1.1.1',443),'http://trusted-gateway:7897')
    conn.close.assert_called_once()
 def test_eof_closes_unfinished_tunnel(self):
  conn=MagicMock();conn.recv.side_effect=[b'H',b'']
  with patch.object(m.socket,'create_connection',return_value=conn):
   with self.assertRaises(OSError):m.upstream_tunnel(('1.1.1.1',443),'http://trusted-gateway:7897')
  conn.close.assert_called_once()
 def test_oversized_response_header_is_rejected_and_closed(self):
  conn=MagicMock();conn.recv.return_value=b'x'
  with patch.object(m.socket,'create_connection',return_value=conn):
   with self.assertRaises(OSError):m.upstream_tunnel(('1.1.1.1',443),'http://trusted-gateway:7897')
  self.assertLessEqual(conn.recv.call_count,32769)
  conn.close.assert_called_once()
 def test_public_destination_is_numeric_and_never_reresolved(self):
  conn=MagicMock();conn.recv.side_effect=[bytes([x]) for x in b'HTTP/1.1 200 Connection established\r\n\r\n']
  answer=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('1.1.1.1',443))]
  with patch.dict(os.environ,{'PUBLIC_EGRESS_UPSTREAM_PROXY':'http://trusted-gateway:7897'}),patch.object(m.socket,'socket'),patch.object(m.socket,'getaddrinfo',return_value=answer) as resolve,patch.object(m.socket,'create_connection',return_value=conn) as dial:
   self.assertIs(m.connect('public.example',443),conn)
   resolve.assert_called_once_with('public.example',443,type=socket.SOCK_STREAM)
   dial.assert_called_once_with(('trusted-gateway',7897),timeout=15)
   conn.sendall.assert_called_once_with(b'CONNECT 1.1.1.1:443 HTTP/1.1\r\nHost: 1.1.1.1:443\r\n\r\n')
 def test_mixed_private_answers_still_fail_before_upstream(self):
  answer=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('1.1.1.1',443)),(socket.AF_INET,socket.SOCK_STREAM,6,'',('192.168.1.1',443))]
  with patch.dict(os.environ,{'PUBLIC_EGRESS_UPSTREAM_PROXY':'http://trusted-gateway:7897'}),patch.object(m.socket,'socket'),patch.object(m.socket,'getaddrinfo',return_value=answer),patch.object(m.socket,'create_connection') as dial:
   with self.assertRaises(ValueError):m.connect('mixed.example',443)
   dial.assert_not_called()
 def test_failed_upstream_response_is_closed(self):
  conn=MagicMock();conn.recv.side_effect=[bytes([x]) for x in b'HTTP/1.1 407 Authentication required\r\n\r\n']
  answer=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('1.1.1.1',443))]
  with patch.dict(os.environ,{'PUBLIC_EGRESS_UPSTREAM_PROXY':'http://trusted-gateway:7897'}),patch.object(m.socket,'socket'),patch.object(m.socket,'getaddrinfo',return_value=answer),patch.object(m.socket,'create_connection',return_value=conn):
   with self.assertRaises(OSError):m.connect('public.example',443)
   conn.close.assert_called_once()
