"""Generic pre-step barrier. Only controller-authenticated input can release it."""
import json
import os
from pathlib import Path
import stat
import time


def check(root, environment):
    info = root.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o755:
        raise ValueError('untrusted input barrier')
    if not os.statvfs(root).f_flag & os.ST_RDONLY: raise ValueError('input barrier must be read-only')
    fd = os.open(root/'ready.json', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o444 or not 0 < info.st_size <= 4096:
            raise ValueError('untrusted input binding')
        record = json.loads(os.read(fd, 4097))
    finally: os.close(fd)
    if set(record) != {'schema','repository','run_id','attempt','job_id'} or record['schema'] != 'ci-job-input-ready/v1':
        raise ValueError('unknown input binding')
    if record['repository'] != environment['GITHUB_REPOSITORY'] or str(record['run_id']) != environment['GITHUB_RUN_ID'] or str(record['attempt']) != environment['GITHUB_RUN_ATTEMPT'] or type(record['job_id']) is not int or record['job_id'] <= 0:
        raise ValueError('foreign input binding')


def main():
    end = time.monotonic() + 900
    while True:
        try:
            check(Path('/run/ci-input-gate'), os.environ)
            return
        except FileNotFoundError:
            if time.monotonic() >= end: raise SystemExit('trusted job input deadline exceeded')
            time.sleep(1)


if __name__ == '__main__': main()
