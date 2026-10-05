#!/usr/bin/env python3
"""Create a persistent CI cache; optional verified migration, no downloads."""
import argparse
import json
import re
from pathlib import Path
import subprocess
import uuid

DEFAULT_IMAGE = 'ghcr.io/creekxi2026/ci-runner-runner@sha256:fd006fc20579487c9f23bf248f97e8c9ce3e49bf31a6387cb40507391dfd4f9f'


def run(*args): return subprocess.check_output(args).decode()


def ensure_volume(args):
    if not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{1,127}', args.volume):
        raise ValueError('Invalid volume name')
    if not re.fullmatch(r'https://github.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', args.repository):
        raise ValueError('Exact GitHub repository URL required')
    if not re.fullmatch(r'[a-z0-9][a-z0-9_-]{0,63}', args.lane) or not args.owner:
        raise ValueError('Explicit deployment owner and trust lane required')
    labels = {'ci-runner.cache-schema':'v2-linux-arm64-trusted-manual',
              'ci-runner.cache-owner':args.owner,
              'ci-runner.cache-repository':args.repository,
              'ci-runner.cache-lane':args.lane}
    # Fail before creating the volume if the explicitly selected local image is absent.
    image = json.loads(run('docker', 'image', 'inspect', args.image))[0]
    if image['Os'] != 'linux': raise ValueError('A Linux runner image is required')
    if args.import_volume:
        if args.import_volume == args.volume: raise ValueError('Source equals destination')
        source = json.loads(run('docker', 'volume', 'inspect', args.import_volume))[0]
        if source['Driver'] != 'local' or source.get('Options') or not (source.get('Labels') or {}).get('ci-runner.cache-schema'):
            raise ValueError('Source must be an explicitly managed CI cache volume')
        if run('docker', 'ps', '-q', '--filter', 'volume='+args.import_volume).strip():
            raise ValueError('Source cache is busy; stop its jobs before migration')
    names = run('docker', 'volume', 'ls', '--format', '{{.Name}}').splitlines()
    if args.volume not in names:
        cmd = ['docker', 'volume', 'create', '--driver', 'local']
        for k, v in labels.items(): cmd += ['--label', k+'='+v]
        run(*cmd, args.volume)
    info = json.loads(run('docker', 'volume', 'inspect', args.volume))[0]
    if info['Driver'] != 'local' or info.get('Options') or info.get('Labels') != labels:
        raise ValueError('Existing volume identity mismatch; refusing initialization')
    name = 'ci-cache-init-'+uuid.uuid4().hex[:10]
    cmd = ['docker', 'run', '--name', name, '--network', 'none', '--read-only', '--user', '0',
           '--cap-drop=ALL', '--cap-add=CHOWN', '--cap-add=FOWNER', '--cap-add=DAC_OVERRIDE',
           '--security-opt=no-new-privileges', '--memory', '1g', '--memory-swap', '1g', '--cpus', '2',
           '--entrypoint', 'python3', '--mount', 'type=volume,src='+args.volume+',dst=/cache,volume-nocopy',
           '--mount', 'type=bind,src='+str(Path(__file__).resolve().parent)+',dst=/profile,readonly',
           '--mount', 'type=bind,src='+str(Path(__file__).resolve().parents[2])+',dst=/opt/ci,readonly']
    if args.import_volume:
        cmd += ['--mount', 'type=volume,src='+args.import_volume+',dst=/source,readonly,volume-nocopy']
    cmd += [args.image, '/profile/init-volume.py']
    if args.import_volume: cmd += ['--source', '/source/data']
    for rename in args.rename: cmd += ['--rename', rename]
    try:
        subprocess.run(cmd, check=True, timeout=300)
    finally:
        subprocess.run(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL, check=True)
    print('Persistent volume retained:', args.volume)


if __name__ == '__main__':
    p = argparse.ArgumentParser()
    p.add_argument('--volume', default='ci-deps-linux-arm64')
    p.add_argument('--owner', required=True, help='Controller DEPLOYMENT_ID')
    p.add_argument('--repository', required=True, help='Controller GITHUB_CONFIG_URL')
    p.add_argument('--lane', required=True, help='Controller DEPENDENCY_CACHE_TRUST_LANE')
    p.add_argument('--image', default=DEFAULT_IMAGE)
    p.add_argument('--import-volume', help='Idle trusted CI cache to copy and verify once')
    p.add_argument('--rename', action='append', default=[], help='cache/old=cache/new namespace mapping on import')
    ensure_volume(p.parse_args())
