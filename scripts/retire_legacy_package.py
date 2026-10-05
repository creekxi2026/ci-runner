#!/usr/bin/env python3
"""Explicitly retire only this repository's obsolete monolithic GHCR package."""
import json
import os
import subprocess

REPOSITORY = 'creekxi2026/ci-runner'
PATH = 'users/creekxi2026/packages/container/ci-runner'


def retirement_path(repository, metadata):
    if (repository != REPOSITORY
            or metadata.get('name') != 'ci-runner'
            or metadata.get('package_type') != 'container'
            or (metadata.get('owner') or {}).get('login') != 'creekxi2026'
            or (metadata.get('repository') or {}).get('full_name') != REPOSITORY):
        raise ValueError('refusing to retire a package outside the exact legacy scope')
    return PATH


if __name__ == '__main__':
    if os.environ.get('RETIRE_CONFIRMATION') != 'delete-ci-runner-legacy-package':
        raise ValueError('explicit legacy retirement confirmation is required')
    metadata = json.loads(subprocess.check_output(['gh', 'api', PATH], text=True))
    target = retirement_path(os.environ.get('GITHUB_REPOSITORY'), metadata)
    subprocess.run(['gh', 'api', '--method', 'DELETE', target], check=True)
    print('Retired exact legacy package:', target)
