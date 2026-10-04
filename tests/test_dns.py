import importlib.util, pathlib, socket, unittest
from unittest.mock import patch
spec=importlib.util.spec_from_file_location('egress',pathlib.Path(__file__).resolve().parents[1]/'egress.py')
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
class DNS(unittest.TestCase):
 def test_mixed_public_private_answers_fail_closed(self):
  answers=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('1.1.1.1',443)),(socket.AF_INET,socket.SOCK_STREAM,6,'',('127.0.0.1',443))]
  with patch.object(m.socket,'getaddrinfo',return_value=answers),patch.object(m.socket,'socket') as dial:
   with self.assertRaises(ValueError):m.connect('rebinding.example',443)
   dial.assert_not_called()
 def test_verified_numeric_address_is_dialed_without_reresolve(self):
  answers=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('1.1.1.1',443))]
  with patch.object(m.socket,'getaddrinfo',return_value=answers) as dns,patch.object(m.socket,'socket') as dial:
   m.connect('rebinding.example',443)
   dns.assert_called_once()
   dial.return_value.connect.assert_called_once_with(('1.1.1.1',443))
 def test_non_http_ports_fail_closed(self):
  with self.assertRaises(ValueError):m.connect('github.com',22)
