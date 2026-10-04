import importlib.util
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("release_env", ROOT / "scripts/release_env.py")


class ReleaseEnvironment(unittest.TestCase):
    def module(self):
        self.assertTrue(pathlib.Path(SPEC.origin).is_file(), "atomic release writer is missing")
        module = importlib.util.module_from_spec(SPEC)
        SPEC.loader.exec_module(module)
        return module

    def test_complete_pair_is_digest_pinned(self):
        module = self.module()
        meta = {name: {"containerimage.digest": "sha256:" + char * 64}
                for name, char in (("controller", "a"), ("runner", "b"), ("postgres", "d"))}
        result = module.render("creekxi2026/ci-runner", "c" * 40, meta)
        self.assertIn("CONTROLLER_IMAGE=ghcr.io/creekxi2026/ci-runner@sha256:" + "a" * 64, result)
        self.assertIn("RUNNER_IMAGE=ghcr.io/creekxi2026/ci-runner@sha256:" + "b" * 64, result)
        self.assertNotIn(":controller\n", result)
        self.assertIn("IMAGE_REVISION=" + "c" * 40, result)
        self.assertIn("POSTGRES_IMAGE=ghcr.io/creekxi2026/ci-runner@sha256:" + "d" * 64, result)

    def test_partial_publication_cannot_produce_deployment(self):
        module = self.module()
        with self.assertRaises(ValueError):
            module.render("creekxi2026/ci-runner", "c" * 40,
                          {"controller": {"containerimage.digest": "sha256:" + "a" * 64}})

    def test_invalid_digest_or_revision_fails_closed(self):
        module = self.module()
        for revision, digest in (("main", "sha256:" + "a" * 64),
                                 ("c" * 40, "latest"), ("c" * 40, "sha256:" + "a" * 63)):
            with self.subTest(revision=revision, digest=digest):
                with self.assertRaises(ValueError):
                    module.render("creekxi2026/ci-runner", revision,
                                  {name: {"containerimage.digest": digest}
                                   for name in ("controller", "runner")})


if __name__ == "__main__":
    unittest.main()
