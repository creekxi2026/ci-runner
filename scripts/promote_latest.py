#!/usr/bin/env python3
"""Validate the complete candidate set before promoting any latest tag."""
import json
import pathlib
import subprocess
import sys

from release_env import TARGETS, image_refs


def promote(repository, revision, metadata, run=subprocess.run):
    refs = image_refs(repository, revision, metadata)
    for target in TARGETS:
        source = refs[target]
        package = source.partition("@")[0]
        run(["docker", "buildx", "imagetools", "create", "--prefer-index=false",
             "--tag", package + ":latest", source], check=True)


if __name__ == "__main__":
    repository, revision, directory = sys.argv[1:]
    root = pathlib.Path(directory)
    metadata = {target: json.loads((root / f"{target}.json").read_text())
                for target in TARGETS}
    promote(repository, revision, metadata)
