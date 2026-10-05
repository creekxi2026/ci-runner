#!/usr/bin/env python3
"""Verify a single auditable change against the pinned upstream distribution."""
import argparse
import hashlib
import json
from pathlib import Path

ORIGINAL = '''return Promise.race([promise, timeoutPromise]).then(result => {
        clearTimeout(timeoutHandle);
        return result;
    });'''
FIXED = '''return Promise.race([promise, timeoutPromise]).finally(() => {
        clearTimeout(timeoutHandle);
    });'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true', help='Apply to a freshly downloaded, hash-verified upstream setup bundle')
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1] / 'actions/setup-go'
    upstream = json.loads((root / 'upstream.json').read_text())
    for rel, expected in upstream['files'].items():
        p = root / rel
        data = p.read_bytes()
        if rel == 'dist/setup/index.js':
            if args.apply:
                assert hashlib.sha256(data).hexdigest() == expected, 'upstream bundle hash mismatch'
                assert data.count(ORIGINAL.encode()) == 1, 'unexpected patch location'
                data = data.replace(ORIGINAL.encode(), FIXED.encode())
                p.write_bytes(data)
            assert data.count(FIXED.encode()) == 1, 'expected one timer fix'
            data = data.replace(FIXED.encode(), ORIGINAL.encode())
        assert hashlib.sha256(data).hexdigest() == expected, 'unexpected changes in '+rel
    print('Verified: only timer settlement cleanup differs from actions/setup-go@'+upstream['revision'])


if __name__ == '__main__':
    main()
