#!/usr/bin/env python3
"""Publish an immutable dependency snapshot from an explicitly reviewed idle CI cache.

Never reads Agent Runtime storage. No credentials, worktrees or node_modules are
selected: only npm _cacache, Go module cache and the exact Go build namespace.
"""
import argparse
import json
from pathlib import Path
import re
import subprocess
import uuid


def output(*cmd):
    return subprocess.check_output(cmd, text=True)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source-volume', required=True)
    p.add_argument('--work-volume', default='ci-work-linux-arm64')
    p.add_argument('--image', required=True, help='Verified local runner image with workspace initializer')
    p.add_argument('--go-namespace', required=True)
    p.add_argument('--provenance', required=True, help='Reviewed revision/lock digests and why this source is trusted')
    args = p.parse_args()
    for name in [args.source_volume, args.work_volume]:
        if not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{1,127}', name):
            raise ValueError('invalid volume name')
    for name in [args.source_volume, args.work_volume]:
        v = json.loads(output('docker', 'volume', 'inspect', name))[0]
        if v['Driver'] != 'local' or v.get('Options'):
            raise ValueError('only plain local Docker volumes supported')
        if name == args.source_volume and v.get('Labels', {}).get('ci-runner.cache-schema') != 'v2-linux-arm64-trusted-manual':
            raise ValueError('source must be reviewed trusted-manual CI cache')
    if args.source_volume == args.work_volume:
        raise ValueError('source cache and work volume must differ')
    if output('docker', 'ps', '-q', '--filter', 'volume='+args.source_volume).strip():
        raise ValueError('source cache is in use; stop scheduling its jobs before importing')
    name = 'ci-seed-publish-'+uuid.uuid4().hex[:10]
    # Controller owns the templates root. No whole workspace mount is given to
    # this helper, only protected templates and read-only cache data.
    cmd = ['docker', 'run', '--name', name, '--network', 'none', '--read-only', '--user', '0',
           '--cap-drop', 'ALL', '--cap-add', 'CHOWN', '--cap-add', 'DAC_OVERRIDE', '--cap-add', 'FOWNER',
           '--security-opt', 'no-new-privileges', '--memory', '512m', '--memory-swap', '512m', '--cpus', '1',
           '--mount', f'type=volume,src={args.work_volume},dst=/templates,volume-subpath=templates,volume-nocopy',
           '--mount', f'type=volume,src={args.source_volume},dst=/source,readonly,volume-subpath=data,volume-nocopy',
           '--entrypoint', 'python3', args.image, '/opt/ci/workspace-init.py', 'import',
           '--go-namespace', args.go_namespace, '--provenance', args.provenance]
    try:
        subprocess.run(cmd, check=True, timeout=600)
    finally:
        subprocess.run(['docker', 'rm', '-f', name], check=True, stdout=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
