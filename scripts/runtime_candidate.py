#!/usr/bin/env python3
"""Reuse an unchanged, verified runner image for controller-only PR validation.

This is artifact selection, not a test-result cache. All candidate controller
tests still run. A changed Dockerfile or image input requires a complete build.
"""
import argparse
import json
import subprocess

# Successful images release at this revision; never resolve a mutable tag.
REVISION = "87ce2cfb6bbc9f430c147f05c803868b78032021"
IMAGE = "ghcr.io/creekxi2026/ci-runner-runner@sha256:dd3a684b725a3629798120992b85a93aff44313ce27733057ce9fa9cb86f36aa"
SOURCE = "https://github.com/creekxi2026/ci-runner"


def unchanged_runner_inputs(revision=REVISION):
    # This exemption is valid only for the exact reviewed Dockerfile. Its
    # runner stage copies root-level inputs plus immutable tools/certificates;
    # the Go-built controller binary is copied only into the controller stage.
    if subprocess.run(["git", "diff", "--quiet", revision, "--", "Dockerfile"]).returncode:
        return False
    entries = subprocess.check_output(["git", "ls-tree", "-z", revision]).decode().split("\0")
    old = [entry.split("\t", 1)[1] for entry in entries if entry and entry.split()[1] == "blob"]
    current = subprocess.check_output(["git", "ls-files", "-z"]).decode().split("\0")
    non_inputs = {"README.md", "AGENTS.md", "compose.yaml", ".env.example"}
    inputs = sorted(name for name in set(old + current)
                    if name and "/" not in name and not name.endswith(".go")
                    and name not in non_inputs)
    return subprocess.run(["git", "diff", "--quiet", revision, "--", *inputs]).returncode == 0


def verify_image(metadata):
    labels = metadata.get("Config", {}).get("Labels", {}) or {}
    if (metadata.get("Os") != "linux" or metadata.get("Architecture") != "arm64"
            or labels.get("org.opencontainers.image.revision") != REVISION
            or labels.get("org.opencontainers.image.source") != SOURCE):
        raise ValueError("verified runner image platform/provenance mismatch")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-revision", required=True)
    args = parser.parse_args()
    if unchanged_runner_inputs():
        print("Runner inputs unchanged; reusing verified digest. All runtime tests still execute.", flush=True)
        if subprocess.run(["docker", "pull", IMAGE]).returncode == 0:
            metadata = json.loads(subprocess.check_output(["docker", "image", "inspect", IMAGE]))[0]
            verify_image(metadata)
            subprocess.run(["docker", "tag", IMAGE, "runtime-candidate:review"], check=True)
            return
        print("Verified image unavailable; performing full candidate build.", flush=True)
    else:
        print("Runner inputs changed; performing full candidate build.", flush=True)
    subprocess.run(["docker", "build", "--target", "runner", "--build-arg",
                    "SOURCE_REVISION=" + args.source_revision,
                    "-t", "runtime-candidate:review", "."], check=True)


if __name__ == "__main__":
    main()
