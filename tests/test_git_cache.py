import concurrent.futures
import importlib.util
import json
import os
from pathlib import Path
import shutil
import shlex
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("git_cache", Path(__file__).resolve().parents[1] / "git-cache.py")
cache_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cache_module)


class GitCacheTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.old_umask = os.umask(0o077)
        self.addCleanup(os.umask, self.old_umask)
        self.remote = self.root / "remote"
        self.remote.mkdir()
        self.run_git(self.remote, "init", "-b", "main")
        self.first = self.commit("first")
        cache_parent = os.environ.get("CI_GIT_CACHE_TEST_ROOT")
        if cache_parent:
            cache_temp = tempfile.TemporaryDirectory(dir=cache_parent)
            self.addCleanup(cache_temp.cleanup)
            self.cache_root = Path(cache_temp.name)
        else:
            self.cache_root = self.root / "cache"
            self.cache_root.mkdir(mode=0o700)
        self.cache = cache_module.GitCache(self.cache_root, "example/project", ["refs/heads/main"],
                                           uid=os.getuid(), job_uid=os.getuid())
        self.cache.url = self.remote.as_uri()
        # Only the local fixture enables file transport. Production's explicit
        # protocol allowlist still rejects file:// even if a config is poisoned.
        original = subprocess.Popen

        def local_transport(command, *args, **kwargs):
            if command[0] == "git" and "protocol.allow=never" in command:
                command = command[:1] + ["-c", "protocol.file.allow=always"] + command[1:]
            return original(command, *args, **kwargs)

        patcher = patch.object(cache_module.subprocess, "Popen", side_effect=local_transport)
        patcher.start()
        self.addCleanup(patcher.stop)

    def run_git(self, directory, *args):
        return subprocess.check_output(["git", "-C", str(directory), *args], stderr=subprocess.DEVNULL).decode().strip()

    def commit(self, text):
        (self.remote / "value").write_text(text)
        self.run_git(self.remote, "add", ".")
        self.run_git(self.remote, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", text)
        return self.run_git(self.remote, "rev-parse", "HEAD")

    def prepare(self, name, **kwargs):
        home = self.root / name
        home.mkdir(mode=0o700)
        result = self.cache.prepare(home, **kwargs)
        workspace = home / "_work/project/project"
        self.assertFalse((workspace / "value").exists(), "controller must not expand source blobs")
        self.run_git(workspace, "reset", "--hard", "HEAD")
        return workspace, result

    def test_cold_warm_incremental_exact_old_sha_and_private_write(self):
        a, cold = self.prepare("job-a")
        b, warm = self.prepare("job-b", refresh_seconds=0)
        self.assertFalse(cold["cache_hit"])
        self.assertTrue(warm["cache_hit"])
        self.assertGreater(cold["received_pack_bytes"], 0)
        self.assertEqual(warm["received_pack_bytes"], 0)
        second = self.commit("second")
        c, incremental = self.prepare("job-c", refresh_seconds=0)
        self.assertGreater(incremental["received_pack_bytes"], 0)
        self.assertEqual(second, self.run_git(c, "rev-parse", "HEAD"))
        self.run_git(c, "checkout", "--detach", self.first)
        self.assertEqual((c / "value").read_text(), "first")
        # main moved, and the original source branch can disappear; a pinned
        # ancestor and its complete history remain usable in a private copy.
        self.run_git(self.remote, "branch", "source", self.first)
        self.run_git(self.remote, "branch", "-D", "source")
        self.run_git(c, "merge-base", "--is-ancestor", self.first, second)
        self.assertEqual(self.run_git(c, "diff", self.first, second, "--", "value").count("+second"), 1)
        packs = list((a / ".git/objects/pack").glob("*.pack"))
        original = packs[0].name
        (a / ".git/objects/pack" / original).write_bytes(b"hostile job mutation")
        self.run_git(b, "fsck", "--full")
        self.cache.current()
        self.assertEqual(self.run_git(b, "rev-parse", "HEAD"), self.first)
        self.assertFalse((a / ".git/objects/info/alternates").exists())
        self.assertEqual(len([p for p in self.cache_root.iterdir() if cache_module.SNAPSHOT.fullmatch(p.name)]), 1)

    def test_parallel_distinct_branches_keep_independent_heads(self):
        self.run_git(self.remote, "checkout", "-b", "feature")
        feature = self.commit("feature")
        self.run_git(self.remote, "checkout", "main")
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(lambda name: self.prepare(name), ["one", "two"]))
        a, b = results[0][0], results[1][0]
        self.run_git(b, "fetch", "origin", feature)
        self.run_git(b, "checkout", "--detach", feature)
        self.assertEqual(self.run_git(a, "rev-parse", "HEAD"), self.first)
        self.assertEqual(self.run_git(b, "rev-parse", "HEAD"), feature)
        self.assertEqual((a / "value").read_text(), "first")
        self.assertEqual((b / "value").read_text(), "feature")

    def test_actions_checkout_reset_fetch_and_detach_preserves_objects(self):
        workspace, _ = self.prepare("job")
        # Pinned upstream checkout's cleanup and fetch sequence. Full action
        # execution on native Actions is a separate rollout acceptance gate.
        pack = next((workspace / ".git/objects/pack").glob("*.pack"))
        before = pack.read_bytes()
        self.run_git(workspace, "clean", "-ffdx")
        self.run_git(workspace, "reset", "--hard", "HEAD")
        self.run_git(workspace, "fetch", "--no-tags", "--depth=1", "origin", self.first)
        self.run_git(workspace, "checkout", "--force", "--detach", self.first)
        self.assertEqual(pack.read_bytes(), before)
        self.assertEqual(self.run_git(workspace, "rev-parse", "HEAD"), self.first)

    def test_gc_after_force_push_does_not_remove_live_job_objects(self):
        old = self.commit("old unique commit")
        workspace, _ = self.prepare("live")
        self.run_git(self.remote, "reset", "--hard", self.first)
        new = self.commit("replacement commit")
        self.cache.compact_packs = 1
        other, metrics = self.prepare("after-gc", refresh_seconds=0)
        self.assertIn("gc_seconds", metrics)
        self.assertEqual(self.run_git(other, "rev-parse", "HEAD"), new)
        self.assertEqual(self.run_git(workspace, "rev-parse", "HEAD"), old)
        self.run_git(workspace, "fsck", "--full")
        self.assertEqual((workspace / "value").read_text(), "old unique commit")

    def test_corruption_permissions_and_foreign_metadata_fail_without_export(self):
        self.prepare("first")
        snapshot, _ = self.cache.current()
        pack = next((snapshot / "objects/pack").glob("*.pack"))
        original = pack.read_bytes()
        for mutation, restore in [
            (lambda: pack.chmod(0o666), lambda: pack.chmod(0o600)),
            (lambda: pack.write_bytes(b"corrupt"), lambda: pack.write_bytes(original)),
            (lambda: (snapshot / "objects/info/alternates").write_text("/elsewhere"), lambda: (snapshot / "objects/info/alternates").unlink()),
            (lambda: (snapshot / "config").write_text("[include]\npath=/hostile"), lambda: (snapshot / "config").write_bytes(cache_module.CONFIG)),
        ]:
            mutation()
            with self.assertRaises(ValueError):
                self.cache.prepare(self.root / "not-exported")
            self.assertFalse((self.root / "not-exported").exists())
            restore()

    def test_failed_refresh_uses_valid_snapshot_and_recovers_staging(self):
        self.prepare("old")
        self.cache.url = (self.root / "unavailable").as_uri()
        path, result = self.prepare("offline", refresh_seconds=0)
        self.assertTrue(result["refresh_failed"])
        self.assertEqual(self.run_git(path, "rev-parse", "HEAD"), self.first)
        self.assertFalse((self.cache_root / "staging").exists())
        # Simulate interrupted publication; it is discarded under the same lock.
        (self.cache_root / "staging").mkdir(mode=0o700)
        (self.cache_root / "staging/partial").write_text("partial")
        self.cache.url = self.remote.as_uri()
        self.prepare("recovered", refresh_seconds=0)
        self.assertFalse((self.cache_root / "staging").exists())

    def test_export_failure_cleans_private_repository(self):
        self.prepare("old")
        original = self.cache.git

        def fail(directory, *args, **kwargs):
            if args[:3] == ("update-ref", "--no-deref", "HEAD"):
                raise InterruptedError("cancelled")
            return original(directory, *args, **kwargs)

        with patch.object(self.cache, "git", side_effect=fail):
            with self.assertRaises(InterruptedError):
                self.prepare("cancelled")
        self.assertFalse((self.root / "cancelled/_work/project/project").exists())
        self.cache.current()

    def test_cancelled_refresh_is_not_a_cache_fallback(self):
        self.prepare("old")
        with patch.object(self.cache, "update", side_effect=cache_module.CacheCancelled()):
            with self.assertRaises(cache_module.CacheCancelled):
                self.prepare("cancelled", refresh_seconds=0)
        self.assertFalse((self.root / "cancelled/_work").exists())
        self.cache.current()

    def test_git_timeout_terminates_child_process_group(self):
        fakebin = self.root / "bin"
        fakebin.mkdir()
        pidfile = self.root / "child-pid"
        fakegit = fakebin / "git"
        fakegit.write_text("#!/bin/sh\nsleep 30 &\necho $! > " + shlex.quote(str(pidfile)) + "\nwait\n")
        fakegit.chmod(0o700)
        self.cache.timeout = 1.0
        child = None
        try:
            with patch.dict(os.environ, {"PATH": str(fakebin) + ":" + os.environ["PATH"]}):
                with self.assertRaises(subprocess.TimeoutExpired):
                    self.cache.git(self.root, "version")
            child = int(pidfile.read_text())
            # A killed orphan may briefly be a zombie awaiting PID 1. It must
            # never remain runnable after the Git process group was reaped.
            status = Path(f"/proc/{child}/stat")
            for _ in range(20):
                try:
                    os.kill(child, 0)
                except ProcessLookupError:
                    return
                if status.exists() and status.read_text().split()[2] == "Z":
                    return
                time.sleep(0.01)
            self.fail("Git child survived cancellation")
        finally:
            if child:
                try:
                    os.kill(child, signal.SIGKILL)
                except ProcessLookupError:
                    pass

    def test_real_sigterm_during_refresh_reaps_git_and_cleans_staging(self):
        self.prepare("old")
        fakebin = self.root / "bin"
        fakebin.mkdir()
        marker = self.root / "git-pid"
        script = fakebin / "git"
        script.write_text("#!/bin/sh\nfor arg in \"$@\"; do\n"
                          " if [ \"$arg\" = fetch ]; then\n"
                          "  echo $$ > " + shlex.quote(str(marker)) + "\n  exec sleep 30\n fi\ndone\n"
                          "exec " + shlex.quote(shutil.which("git")) + " \"$@\"\n")
        script.chmod(0o700)
        code = f'''
import importlib.util, os, signal, sys
from pathlib import Path
spec = importlib.util.spec_from_file_location("cache", {str(spec.origin)!r})
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
def stop(*_):
    raise m.CacheCancelled()
signal.signal(signal.SIGTERM, stop)
os.umask(0o077)
c = m.GitCache({str(self.cache_root)!r}, "example/project", ["refs/heads/main"], uid=os.getuid(), job_uid=os.getuid())
try:
    c.prepare({str(self.root / "cancelled")!r}, refresh_seconds=0)
except m.CacheCancelled:
    sys.exit(23)
'''
        proc = subprocess.Popen([sys.executable, "-c", code], env={**os.environ, "PATH": str(fakebin) + ":" + os.environ["PATH"]},
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        try:
            for _ in range(200):
                if marker.exists() or proc.poll() is not None:
                    break
                time.sleep(0.01)
            self.assertTrue(marker.exists(), proc.stderr.read().decode() if proc.poll() is not None else "fetch did not start")
            git_pid = int(marker.read_text())
            proc.send_signal(signal.SIGTERM)
            self.assertEqual(proc.wait(timeout=5), 23)
            with self.assertRaises(ProcessLookupError):
                os.kill(git_pid, 0)
            self.assertFalse((self.cache_root / "staging").exists())
            self.assertFalse((self.root / "cancelled").exists())
            self.cache.current()
        finally:
            if marker.exists():
                # Also reclaim the fixture when testing a broken cancellation
                # implementation; killing only Python could leave its session.
                try:
                    os.killpg(int(marker.read_text()), signal.SIGKILL)
                except ProcessLookupError:
                    pass
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            proc.stderr.close()

    def test_copy_fallback_and_capacity_limits(self):
        self.prepare("old")
        import errno
        with patch.object(cache_module.fcntl, "ioctl", side_effect=OSError(errno.EOPNOTSUPP, "no reflink")):
            _, metrics = self.prepare("fallback")
            self.assertGreater(metrics["copied_bytes"], 0)
        self.cache.max_bytes = 1
        with self.assertRaises(ValueError):
            self.prepare("over-budget")
        self.assertFalse((self.root / "over-budget/_work/project/project").exists())


if __name__ == "__main__":
    unittest.main()
