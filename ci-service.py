#!/usr/bin/python3
"""Request a reviewed service; never accept Docker options or relay credentials."""
import argparse
import json
import os
from pathlib import Path
import re
import signal
import stat
import sys
import time

ROOT = Path('/run/ci-services')


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise argparse.ArgumentError(None, 'invalid arguments')


def metadata(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0
                or stat.S_IMODE(info.st_mode) != 0o444 or info.st_nlink != 1
                or not 0 < info.st_size <= 65536):
            raise ValueError('invalid descriptor')
        with os.fdopen(fd, closefd=False) as stream:
            return json.load(stream)
    finally:
        os.close(fd)


def run(argv):
    # Parse exec's trailing command separately, preserving its arguments verbatim.
    command = []
    if '--' in argv:
        i = argv.index('--')
        argv, command = argv[:i], argv[i + 1:]
    parser = Parser(add_help=False, exit_on_error=False)
    parser.add_argument('action', choices=['acquire', 'ready', 'exec'])
    parser.add_argument('service')
    parser.add_argument('--timeout', type=int, default=45)
    try:
        args = parser.parse_args(argv)
    except (argparse.ArgumentError, SystemExit):
        return 64
    if not 1 <= args.timeout <= 300 or (args.action == 'exec') != bool(command):
        return 64
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,47}', args.service):
        return 65
    try:
        if (os.environ.get('CI_SERVICES_FILE') != str(ROOT / 'index.json')
                or not os.statvfs(ROOT).f_flag & os.ST_RDONLY):
            return 70
        index = metadata(ROOT / 'index.json')
        if (index['version'] != 1 or index['fingerprint'] != os.environ.get('CI_SERVICES_FINGERPRINT')
                or not re.fullmatch('[a-f0-9]{64}', index['lease_id'])):
            return 70
        item = index['services'].get(args.service)
        if item is None:
            return 65
        ready_path = ROOT / 'services' / args.service / 'ready.json'
        if item['ready_file'] != str(ready_path):
            return 70
        request = Path('/tmp') / ('ci-service-request-' + args.service)
        failed = Path('/tmp') / ('ci-service-failed-' + args.service)
        if args.action != 'ready':
            # Atomic request creation; a malicious job cannot make this CLI write
            # through a symlink into another job or its credential file.
            try:
                fd = os.open(request, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
                os.close(fd)
            except FileExistsError:
                pass
        elif not request.exists():
            return 69
        deadline = time.monotonic() + args.timeout
        while True:
            if failed.exists():
                return 69
            try:
                ready = metadata(ready_path)
            except FileNotFoundError:
                ready = None
            if ready is not None:
                if ready['state'] != 'ready' or ready['service_id'] != args.service:
                    return 70
                for key in ('version', 'lease_id', 'owner_id', 'job_id', 'fingerprint'):
                    if ready[key] != index[key]:
                        return 70
                for key in ('image', 'adapter_image', 'config_digest'):
                    if ready[key] != item[key]:
                        return 70
                if args.action == 'exec':
                    os.environ['CI_SERVICE_FILE'] = str(ready_path)
                    os.execvp(command[0], command)
                if args.action == 'acquire':
                    print(ready_path)
                return 0
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return 124
            time.sleep(min(0.1, remaining))
    except (OSError, ValueError, KeyError, TypeError):
        return 70


if __name__ == '__main__':
    def interrupted(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    try:
        status = run(sys.argv[1:])
    except KeyboardInterrupt:
        status = 130
    if status:
        print(f'ci-service: status {status}', file=sys.stderr)
    sys.exit(status)
