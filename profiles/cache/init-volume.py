#!/usr/bin/env python3
"""Initialize a general CI cache or import an idle, explicitly selected cache."""
import argparse
import importlib.util
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import tempfile

SCHEMA = 'ci-cache-v1'
CACHE_NAMES = ('npm', 'gomod', 'pip', 'go-build', 'jest')


def inventory(root):
    files = {}
    for p in sorted(root.rglob('*')):
        rel = str(p.relative_to(root))
        if rel == 'ready.json':
            continue
        if p.is_symlink():
            files[rel] = {'symlink': os.readlink(p)}
        elif p.is_file():
            h = hashlib.sha256()
            with p.open('rb') as f:
                for chunk in iter(lambda: f.read(1024 * 1024), b''): h.update(chunk)
            files[rel] = {'sha256': h.hexdigest(), 'bytes': p.stat().st_size}
    return files


def initialize(root, source, renames):
    lock = os.open(root/'.initialize.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        target = root/'data'
        if target.is_symlink(): raise ValueError('Cache root cannot be a symlink')
        if target.exists():
            ready = json.loads((target/'ready.json').read_text())
            if ready['schema'] != SCHEMA or ready['trust_domain'] != 'trusted':
                raise ValueError('Existing cache identity mismatch')
            print(json.dumps({'status': 'already-initialized', 'schema': SCHEMA}))
            return
        expected = None
        if source:
            if source.is_symlink() or not (source/'ready.json').is_file():
                raise ValueError('Import requires an initialized cache')
            expected = inventory(source)
        with tempfile.TemporaryDirectory(prefix='.prepare-', dir=root) as temp:
            stage = Path(temp)/'data'
            if source: shutil.copytree(source, stage, symlinks=True)
            else: stage.mkdir(mode=0o755)
            for name in CACHE_NAMES:
                d = stage/name
                if d.is_symlink(): raise ValueError('Cache directory cannot be a symlink')
                d.mkdir(mode=0o700, exist_ok=True)
                d.chmod(0o700)
                for p in [d, *d.rglob('*')]:
                    if p.is_symlink(): raise ValueError('Unexpected package cache symlink')
                    os.chown(p, 1001, 1001)
            (stage/'npm'/'_cacache').mkdir(mode=0o700, exist_ok=True)
            os.chown(stage/'npm'/'_cacache', 1001, 1001)
            tools = stage/'tools'
            if tools.is_symlink(): raise ValueError('Tools root cannot be a symlink')
            tools.mkdir(mode=0o755, exist_ok=True)
            for p in [tools, *tools.rglob('*')]:
                if not p.is_symlink(): p.chmod(p.stat().st_mode & ~0o022)
            if expected is not None and inventory(stage) != expected:
                raise ValueError('Imported cache bytes differ from source')
            for old, new in renames:
                # Only rename one immediate cache namespace, never arbitrary paths.
                a, b = Path(old), Path(new)
                if len(a.parts) != 2 or len(b.parts) != 2 or a.parts[0] != b.parts[0] or a.parts[0] not in CACHE_NAMES or '..' in (*a.parts, *b.parts):
                    raise ValueError('Invalid cache namespace mapping')
                if (stage/b).exists(): raise ValueError('Destination namespace exists')
                (stage/a).rename(stage/b)
            ready = {'schema': SCHEMA, 'cache_uid': 1001, 'cache_gid': 1001,
                     'trust_domain': 'trusted', 'tools': 'root-owned; job mount read-only',
                     'import_verified_files': len(expected) if expected is not None else 0}
            (stage/'ready.json').write_text(json.dumps(ready, indent=2)+'\n')
            os.rename(stage, target)
        print(json.dumps({'status':'ready', **ready}))
    finally:
        os.close(lock)


if __name__ == '__main__':
    p = argparse.ArgumentParser()
    p.add_argument('--source', type=Path)
    p.add_argument('--rename', action='append', default=[])
    args = p.parse_args()
    if args.source:
        initialize(Path('/cache'), args.source, [x.split('=', 1) for x in args.rename])
    spec = importlib.util.spec_from_file_location('cache_init', '/opt/ci/cache-init.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.initialize_volume('/cache')
