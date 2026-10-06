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
                for name, char in (("controller", "a"), ("runner", "b"))}
        result = module.render("creekxi2026/ci-runner", "c" * 40, meta)
        self.assertIn("CONTROLLER_IMAGE=ghcr.io/creekxi2026/ci-runner-controller@sha256:" + "a" * 64, result)
        self.assertIn("RUNNER_IMAGE=ghcr.io/creekxi2026/ci-runner-runner@sha256:" + "b" * 64, result)
        self.assertNotIn(":controller\n", result)
        self.assertIn("IMAGE_REVISION=" + "c" * 40, result)
        self.assertNotIn("POSTGRES_IMAGE=", result)

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


class LatestPromotion(unittest.TestCase):
    def module(self):
        path = ROOT / "scripts/promote_latest.py"
        self.assertTrue(path.is_file(), "validated latest promotion script is missing")
        spec = importlib.util.spec_from_file_location("promote_latest", path)
        module = importlib.util.module_from_spec(spec)
        import sys
        from unittest.mock import patch
        with patch.object(sys, "path", [str(ROOT / "scripts"), *sys.path]):
            spec.loader.exec_module(module)
        return module

    def test_invalid_input_never_starts_any_promotion(self):
        module = self.module()
        valid = {name: {"containerimage.digest": "sha256:" + "a" * 64}
                 for name in ("controller", "runner")}
        cases = [("OWNER/repo", "c" * 40, valid), ("owner/repo", "main", valid)]
        for target in valid:
            for bad in (None, {}, [], "not an object", {"containerimage.digest": None},
                        {"containerimage.digest": 123}, {"containerimage.digest": "latest"},
                        {"containerimage.digest": "sha256:" + "a" * 63}):
                meta = dict(valid)
                if bad is None:
                    del meta[target]
                else:
                    meta[target] = bad
                cases.append(("owner/repo", "c" * 40, meta))
        for repository, revision, meta in cases:
            with self.subTest(repository=repository, revision=revision, meta=meta):
                calls = []
                with self.assertRaises(ValueError):
                    module.promote(repository, revision, meta,
                                   run=lambda *args, **kwargs: calls.append(args))
                self.assertEqual(calls, [])

    def test_promotes_only_latest_from_two_role_digests(self):
        module = self.module()
        meta = {name: {"containerimage.digest": "sha256:" + char * 64}
                for name, char in (("controller", "a"), ("runner", "b"))}
        calls = []
        module.promote("creekxi2026/ci-runner", "c" * 40, meta,
                       run=lambda command, **kwargs: calls.append((command, kwargs)))
        expected = []
        for target, char in (("controller", "a"), ("runner", "b")):
            package = f"ghcr.io/creekxi2026/ci-runner-{target}"
            expected.append((["docker", "buildx", "imagetools", "create", "--prefer-index=false",
                              "--tag", package + ":latest", package + "@sha256:" + char * 64],
                             {"check": True}))
        self.assertEqual(calls, expected)


class PublicationPolicy(unittest.TestCase):
    def test_digest_push_uses_a_container_builder(self):
        workflow = (ROOT / ".github/workflows/images.yml").read_text()
        builder = workflow.index('docker buildx create --driver docker-container --use') if 'docker buildx create --driver docker-container --use' in workflow else -1
        self.assertGreaterEqual(builder, 0, 'digest-only pushes are unsupported by the default docker driver')
        self.assertLess(builder, workflow.index('for target in controller runner; do'))

    def test_serialized_digest_candidates_precede_latest_and_artifact(self):
        workflow = (ROOT / ".github/workflows/images.yml").read_text()
        self.assertIn("concurrency:\n  group: ghcr-publication\n  cancel-in-progress: false", workflow)
        candidate = workflow.split("for target in controller runner; do", 1)[1].split("done", 1)[0]
        self.assertIn('--output "type=image,name=ghcr.io/$GITHUB_REPOSITORY-$target,push-by-digest=true,name-canonical=true,push=true"', candidate)
        self.assertNotIn("--tag", candidate)
        self.assertNotIn("--push", candidate)
        for forbidden in ("$GITHUB_RUN_ID", "$GITHUB_RUN_ATTEMPT", ":$target-", ":latest"):
            self.assertNotIn(forbidden, workflow)
        self.assertIn('--platform linux/arm64 --provenance=false', candidate)
        self.assertIn('--label org.opencontainers.image.revision="$GITHUB_SHA"', candidate)
        promotion = workflow.index('python3 scripts/promote_latest.py "$GITHUB_REPOSITORY" "$GITHUB_SHA" deployment')
        environment = workflow.index('python3 scripts/release_env.py "$GITHUB_REPOSITORY" "$GITHUB_SHA" deployment')
        artifact = workflow.index("uses: actions/upload-artifact@")
        self.assertLess(promotion, environment)
        self.assertLess(environment, artifact)


if __name__ == "__main__":
    unittest.main()
