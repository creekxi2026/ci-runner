import importlib.util, pathlib, unittest
p=pathlib.Path(__file__).resolve().parents[1]/'egress.py'
class Policy(unittest.TestCase):
 def test_private_and_nonpublic_denied(self):
  self.assertTrue(p.exists(), 'public-only egress policy not implemented')
  spec=importlib.util.spec_from_file_location('egress',p); m=importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
  for ip in ['127.0.0.1','10.1.2.3','192.168.1.1','172.17.0.1','169.254.169.254','0.0.0.0','::1','fc00::1','fe80::1','::ffff:192.168.1.1','224.0.0.1','100.64.0.1','2001:db8::1','2002:c0a8:101::','64:ff9b::c0a8:101','2001::c0a8:101']:
   self.assertFalse(m.public(ip),ip)
  self.assertTrue(m.public('1.1.1.1'))
if __name__=='__main__': unittest.main()
