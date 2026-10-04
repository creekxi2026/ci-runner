import pathlib, unittest
class FirewallContract(unittest.TestCase):
 def test_namespace_firewall_denies_unsolicited_input(self):
  text=(pathlib.Path(__file__).resolve().parents[1]/'firewall.sh').read_text()
  self.assertIn('"$cmd" -P INPUT DROP',text)
  self.assertIn('"$cmd" -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT',text)
