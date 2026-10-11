import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("candidate", Path(__file__).resolve().parents[1] / "scripts/runtime_candidate.py")
candidate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(candidate)


class RuntimeCandidateTest(unittest.TestCase):
    def test_only_unchanged_image_inputs_reuse_the_image(self):
        previous = Path.cwd()
        with tempfile.TemporaryDirectory() as directory:
            os.chdir(directory)
            try:
                subprocess.run(["git", "init", "-q"], check=True)
                sources = {"Dockerfile": "reviewed stages", ".dockerignore": "tests",
                           "runner.sh": "runner", "go.mod": "module fixture",
                           "controller.go": "old", "README.md": "old"}
                for name, value in sources.items():
                    Path(name).write_text(value)
                Path("tests").mkdir()
                Path("tests/existing.py").write_text("old test")
                subprocess.run(["git", "add", "."], check=True)
                subprocess.run(["git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                                "commit", "-qm", "baseline"], check=True)
                revision = subprocess.check_output(["git", "rev-parse", "HEAD"]).decode().strip()
                Path("controller.go").write_text("new controller")
                Path("tests/existing.py").write_text("new test")
                Path("README.md").write_text("new docs")
                self.assertTrue(candidate.unchanged_runner_inputs(revision))
                for name in ["Dockerfile", ".dockerignore", "runner.sh", "go.mod"]:
                    with self.subTest(input=name):
                        Path(name).write_text("changed")
                        self.assertFalse(candidate.unchanged_runner_inputs(revision))
                        Path(name).write_text(sources[name])
                Path("runner.sh").unlink()
                self.assertFalse(candidate.unchanged_runner_inputs(revision))
            finally:
                os.chdir(previous)

    def test_wrong_image_platform_or_provenance_is_rejected(self):
        metadata = {"Os": "linux", "Architecture": "arm64", "Config": {"Labels": {
            "org.opencontainers.image.revision": candidate.REVISION,
            "org.opencontainers.image.source": candidate.SOURCE}}}
        candidate.verify_image(metadata)
        metadata["Architecture"] = "amd64"
        with self.assertRaises(ValueError):
            candidate.verify_image(metadata)
        metadata["Architecture"] = "arm64"
        metadata["Config"]["Labels"]["org.opencontainers.image.revision"] = "other"
        with self.assertRaises(ValueError):
            candidate.verify_image(metadata)
