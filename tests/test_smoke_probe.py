import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]


class PublicConnectivityProbe(unittest.TestCase):
    def test_probe_is_fail_closed_and_independent_of_anonymous_api_quota(self):
        workflow = (ROOT / '.github/workflows/smoke.yml').read_text()
        self.assertNotIn('https://api.github.com/zen', workflow)
        self.assertIn('curl -fsS --max-time 30 https://github.com/robots.txt -o /dev/null', workflow)


if __name__ == '__main__':
    unittest.main()
