import pathlib, unittest, tempfile, subprocess, os
class FirewallContract(unittest.TestCase):
 def test_no_database_request_has_no_database_allowance(self):
  root=pathlib.Path(__file__).resolve().parents[1]
  temp=root/'.operations'/'test-temp';temp.mkdir(parents=True,exist_ok=True)
  with tempfile.TemporaryDirectory(dir=temp) as directory:
   for name in ['iptables','ip6tables']:
    executable=pathlib.Path(directory)/name
    executable.write_text('#!/bin/sh\nprintf "%s " "$@"; printf "\\n"\n');executable.chmod(0o700)
   result=subprocess.run(['/bin/sh',str(root/'firewall.sh'),'192.168.100.2'],env={**os.environ,'PATH':directory},text=True,capture_output=True)
   self.assertEqual(result.returncode,0,result.stderr)
   self.assertIn('--dport 3128',result.stdout)
   self.assertNotIn('--dport 5432',result.stdout)
 def test_namespace_firewall_denies_unsolicited_input(self):
  text=(pathlib.Path(__file__).resolve().parents[1]/'firewall.sh').read_text()
  self.assertIn('"$cmd" -P INPUT DROP',text)
  self.assertIn('"$cmd" -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT',text)
