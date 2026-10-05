import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]


class GoProxyDefaults(unittest.TestCase):
    def test_runner_image_defaults_to_cn_proxy_with_checksum_verification(self):
        dockerfile = (ROOT / 'Dockerfile').read_text()
        runner = dockerfile.split(' AS runner\n', 1)[1].split('FROM postgres:', 1)[0]
        self.assertIn('ENV GOPROXY=https://goproxy.cn GOSUMDB=sum.golang.org', runner)

    def test_smoke_exercises_image_defaults_without_workflow_override(self):
        workflow = (ROOT / '.github/workflows/smoke.yml').read_text()
        self.assertNotIn('GOPROXY:', workflow)
        self.assertNotIn('GOSUMDB:', workflow)
        self.assertIn('go env GOPROXY GOSUMDB', workflow)
