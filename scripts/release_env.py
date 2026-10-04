#!/usr/bin/env python3
"""Publish a deployable environment only after BOTH image pushes succeed."""
import json
import pathlib
import re
import sys


def render(repository, revision, metadata):
    if not re.fullmatch(r"[a-z0-9][a-z0-9_.-]*/[a-z0-9][a-z0-9_.-]*", repository):
        raise ValueError("invalid lowercase registry repository")
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("release requires a full source revision")
    refs = {}
    for target in ("controller", "runner"):
        digest = metadata.get(target, {}).get("containerimage.digest", "")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            raise ValueError(f"missing or invalid {target} publication digest")
        refs[target] = f"ghcr.io/{repository}@{digest}"
    template = (pathlib.Path(__file__).resolve().parents[1] / ".env.example").read_text()
    replacements = {"CONTROLLER_IMAGE": refs["controller"], "RUNNER_IMAGE": refs["runner"],
                    "IMAGE_REVISION": revision}
    lines = []
    for line in template.splitlines():
        key = line.partition("=")[0]
        lines.append(f"{key}={replacements[key]}" if key in replacements else line)
    return "\n".join(lines) + "\n"


if __name__ == "__main__":
    repository, revision, directory = sys.argv[1:]
    root = pathlib.Path(directory)
    metadata = {target: json.loads((root / f"{target}.json").read_text())
                for target in ("controller", "runner")}
    result = render(repository, revision, metadata)
    # No partial environment file is produced on validation failure.
    (root / "env.txt").write_text(result)
